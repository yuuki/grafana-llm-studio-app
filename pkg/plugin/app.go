package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/grafana/grafana-llm-app/llmclient"
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/backend/resource/httpadapter"
	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/jobs"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
)

const maxRequestBodyBytes = 256 * 1024

var (
	_ backend.CallResourceHandler   = (*App)(nil)
	_ backend.CheckHealthHandler    = (*App)(nil)
	_ instancemgmt.InstanceDisposer = (*App)(nil)
)

// App is the Grafana backend App Plugin instance for LLM Dashboard Studio.
type App struct {
	settings        Settings
	llmClient       llm.Client
	llmInitErr      error
	jobManager      *jobs.Manager
	resourceHandler backend.CallResourceHandler
}

// NewApp creates a new App instance from Grafana's AppInstanceSettings.
func NewApp(ctx context.Context, appSettings backend.AppInstanceSettings) (instancemgmt.Instance, error) {
	var trustedAppURL string
	var managedSecret string
	if cfg := backend.GrafanaConfigFromContext(ctx); cfg != nil {
		if appURL, err := cfg.AppURL(); err == nil {
			trustedAppURL = strings.TrimSpace(appURL)
		}
		if secret, err := cfg.PluginAppClientSecret(); err == nil {
			managedSecret = strings.TrimSpace(secret)
		}
	}

	settings, err := LoadSettings(appSettings, trustedAppURL)
	if err != nil {
		return nil, err
	}

	// Prefer explicit secureJsonData token; allow Grafana-managed secret only after grafanaUrl origin validation passed.
	token := settings.llmServiceAccountToken
	if token == "" && managedSecret != "" {
		token = managedSecret
	}

	var (
		client  llm.Client
		initErr error
	)
	switch settings.LLMMode {
	case LLMModeMock:
		// Explicit dev/test-only mock client; never silently activated when LLMMode == grafana-llm-app.
		client = &llm.MockClient{}
	case LLMModeGrafanaLLMApp:
		client, initErr = llm.NewGrafanaLLMClient(llm.GrafanaLLMConfig{
			GrafanaURL:          settings.GrafanaURL,
			ServiceAccountToken: token,
			ModelTier:           llmclient.Model(settings.ModelTier),
			MaxOutputTokens:     settings.MaxOutputTokens,
			ReasoningEffort:     settings.EffectiveReasoningEffort,
			RequestTimeout:      settings.LLMTimeout(),
			MaxTransportRetries: 1,
		})
	default:
		initErr = fmt.Errorf("unsupported llmMode: %s", settings.LLMMode)
	}

	return newAppWithClient(settings, client, initErr), nil
}

func newAppWithClient(settings Settings, client llm.Client, initErr error) *App {
	var mgr *jobs.Manager
	if client != nil {
		mgr = jobs.NewManager(client, jobs.Config{
			MaxPanels:            settings.MaxPanels,
			MaxRepairAttempts:    settings.EffectiveMaxRepairs,
			MaxRepairAttemptsSet: true,
			JobDeadline:          settings.JobDeadline(),
			MaxConcurrentJobs:    settings.MaxConcurrentJobs,
			AllowedDSUIDs:        settings.AllowedDatasourceUIDs,
		})
	}
	a := &App{
		settings:   settings,
		llmClient:  client,
		llmInitErr: initErr,
		jobManager: mgr,
	}
	mux := http.NewServeMux()
	a.registerRoutes(mux)
	a.resourceHandler = httpadapter.New(mux)
	return a
}

// Dispose cancels all active in-flight jobs when Grafana recycles the plugin instance.
func (a *App) Dispose() {
	if a.jobManager != nil {
		a.jobManager.Close()
	}
}

// CallResource delegates plugin resource HTTP requests to the internal ServeMux.
func (a *App) CallResource(ctx context.Context, req *backend.CallResourceRequest, sender backend.CallResourceResponseSender) error {
	return a.resourceHandler.CallResource(ctx, req, sender)
}

// CheckHealth verifies configuration and connectivity to grafana-llm-app (or explicit dev mock).
func (a *App) CheckHealth(ctx context.Context, _ *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	if a.llmInitErr != nil || a.llmClient == nil {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: fmt.Sprintf("LLM client not initialized (%s): %v", a.settings.LLMMode, a.llmInitErr),
		}, nil
	}
	if len(a.settings.AllowedDatasourceUIDs) == 0 {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: "No allowed Prometheus datasource UIDs configured. Set allowedDatasourceUids in App Config.",
		}, nil
	}
	enabled, err := a.llmClient.Enabled(ctx)
	if err != nil {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: fmt.Sprintf("grafana-llm-app health check failed: %v", err),
		}, nil
	}
	if !enabled {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: "grafana-llm-app is unavailable, unauthorized, or no LLM provider is enabled/configured.",
		}, nil
	}
	return &backend.CheckHealthResult{
		Status:  backend.HealthStatusOk,
		Message: fmt.Sprintf("LLM Dashboard Studio ready (mode=%s, modelTier=%s, allowedDatasources=%d)", a.settings.LLMMode, a.settings.ModelTier, len(a.settings.AllowedDatasourceUIDs)),
	}, nil
}

func (a *App) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/health", a.handleHealth)
	mux.HandleFunc("/config/public", a.handlePublicConfig)
	mux.HandleFunc("/jobs/generate", a.handleGenerate)
	mux.HandleFunc("/jobs/", a.handleJobAction)
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	pCtx := backend.PluginConfigFromContext(r.Context())
	if _, err := callerOwnerFromContext(r.Context()); err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	isAdmin := pCtx.User != nil && strings.EqualFold(pCtx.User.Role, "Admin")
	if a.llmInitErr != nil || a.llmClient == nil || len(a.settings.AllowedDatasourceUIDs) == 0 {
		msg := "service not ready"
		if isAdmin && a.llmInitErr != nil {
			msg = a.llmInitErr.Error()
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status":  "ERROR",
			"message": msg,
		})
		return
	}
	// Avoid triggering live LLM test completions on non-admin /health probes;
	// only Admin probes call CheckHealth.
	if !isAdmin {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "OK",
		})
		return
	}
	res, _ := a.CheckHealth(r.Context(), &backend.CheckHealthRequest{})
	status := http.StatusOK
	if res.Status != backend.HealthStatusOk {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"status":  res.Status.String(),
		"message": res.Message,
		"llmMode": a.settings.LLMMode,
	})
}

func (a *App) handlePublicConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, err := callerOwnerFromContext(r.Context()); err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.settings.Public())
}

func (a *App) handleGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if a.jobManager == nil {
		writeError(w, http.StatusServiceUnavailable, "LLM client unavailable; check plugin configuration")
		return
	}
	owner, err := callerOwnerFromContext(r.Context())
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	var in jobs.GenerateInput
	if err := decodeBoundedJSON(r.Body, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	snap, err := a.jobManager.Generate(r.Context(), owner, in)
	if err != nil {
		writeJobError(w, err, snap)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (a *App) handleJobAction(w http.ResponseWriter, r *http.Request) {
	if a.jobManager == nil {
		writeError(w, http.StatusServiceUnavailable, "LLM client unavailable; check plugin configuration")
		return
	}
	owner, err := callerOwnerFromContext(r.Context())
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	trimmed := strings.TrimPrefix(r.URL.Path, "/jobs/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) == 0 || len(parts) > 2 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "invalid job route")
		return
	}
	jobID := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch {
	case action == "" && r.Method == http.MethodGet:
		snap, err := a.jobManager.Get(owner, jobID)
		if err != nil {
			writeJobError(w, err, snap)
			return
		}
		writeJSON(w, http.StatusOK, snap)

	case action == "patch" && r.Method == http.MethodPost:
		var in jobs.PatchInput
		if err := decodeBoundedJSON(r.Body, &in); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		snap, err := a.jobManager.Patch(r.Context(), owner, jobID, in)
		if err != nil {
			writeJobError(w, err, snap)
			return
		}
		writeJSON(w, http.StatusOK, snap)

	case action == "repair" && r.Method == http.MethodPost:
		var in jobs.RepairInput
		if err := decodeBoundedJSON(r.Body, &in); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		snap, err := a.jobManager.Repair(r.Context(), owner, jobID, in)
		if err != nil {
			writeJobError(w, err, snap)
			return
		}
		writeJSON(w, http.StatusOK, snap)

	case action == "undo" && r.Method == http.MethodPost:
		snap, err := a.jobManager.Undo(owner, jobID)
		if err != nil {
			writeJobError(w, err, snap)
			return
		}
		writeJSON(w, http.StatusOK, snap)

	case action == "cancel" && r.Method == http.MethodPost:
		snap, err := a.jobManager.Cancel(owner, jobID)
		if err != nil {
			writeJobError(w, err, snap)
			return
		}
		writeJSON(w, http.StatusOK, snap)

	default:
		writeError(w, http.StatusNotFound, "unknown job route or method")
	}
}

// callerOwnerFromContext derives orgId and user identity strictly from Grafana's trusted PluginContext.
// Client-supplied orgId/userId in request bodies or arbitrary headers are NEVER trusted.
func callerOwnerFromContext(ctx context.Context) (jobs.Owner, error) {
	pCtx := backend.PluginConfigFromContext(ctx)
	if pCtx.OrgID <= 0 || pCtx.User == nil || strings.TrimSpace(pCtx.User.Login) == "" {
		return jobs.Owner{}, errors.New("authenticated Grafana user context is required")
	}
	return jobs.Owner{
		OrgID:     pCtx.OrgID,
		UserLogin: pCtx.User.Login,
	}, nil
}

func decodeBoundedJSON(body io.Reader, dst any) error {
	if body == nil {
		return errors.New("request body is required")
	}
	limited := io.LimitReader(body, maxRequestBodyBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read request body: %w", err)
	}
	if len(raw) > maxRequestBodyBytes {
		return fmt.Errorf("request body exceeds max %d bytes", maxRequestBodyBytes)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON request: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.New("unexpected trailing tokens after JSON request body")
	}
	return nil
}

func writeJobError(w http.ResponseWriter, err error, snap jobs.JobSnapshot) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, jobs.ErrJobNotFound):
		status = http.StatusNotFound
	case errors.Is(err, jobs.ErrForbidden), errors.Is(err, jobs.ErrDatasourceDenied):
		status = http.StatusForbidden
	case errors.Is(err, ir.ErrRevisionConflict), errors.Is(err, jobs.ErrOperationInProgress), errors.Is(err, jobs.ErrJobCancelled):
		status = http.StatusConflict
	case errors.Is(err, jobs.ErrBudgetExhausted), errors.Is(err, jobs.ErrConcurrencyLimited):
		status = http.StatusTooManyRequests
	case errors.Is(err, jobs.ErrNotRepairable), errors.Is(err, jobs.ErrNoUndoHistory), errors.Is(err, ir.ErrNoOpPatch):
		status = http.StatusUnprocessableEntity
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
	case errors.Is(err, llm.ErrOutputTruncated), errors.Is(err, llm.ErrOutputRefused):
		status = http.StatusBadGateway
	}
	payload := map[string]any{
		"error": err.Error(),
	}
	if snap.JobID != "" {
		payload["job"] = snap
	}
	writeJSON(w, status, payload)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.DefaultLogger.Error("failed to encode JSON response", "err", err)
	}
}
