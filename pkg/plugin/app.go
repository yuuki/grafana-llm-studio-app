package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

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
	settings             Settings
	llmClient            llm.Client
	llmInitErr           error
	jobStore             jobs.JobStore
	storeInitErr         error
	jobManager           *jobs.Manager
	resourceHandler      backend.CallResourceHandler
	sseHeartbeatInterval time.Duration
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
	if envGrafanaURL := strings.TrimSpace(os.Getenv("LLMSTUDIO_GRAFANA_URL")); envGrafanaURL != "" {
		trustedAppURL = envGrafanaURL
	}
	pinnedRedisAddr := strings.TrimSpace(os.Getenv("LLMSTUDIO_REDIS_ADDR"))
	pinnedPostgresAddr := strings.TrimSpace(os.Getenv("LLMSTUDIO_POSTGRES_ADDR"))

	settings, err := LoadSettingsWithOptions(appSettings, LoadOptions{
		TrustedGrafanaAppURL: trustedAppURL,
		PinnedRedisAddr:      pinnedRedisAddr,
		PinnedPostgresAddr:   pinnedPostgresAddr,
	})
	if err != nil {
		return nil, err
	}

	// Prefer explicit secureJsonData token; allow Grafana-managed secret ONLY when grafanaUrl
	// was strictly pinned against a trusted Grafana base URL.
	token := settings.llmServiceAccountToken
	if token == "" && managedSecret != "" && trustedAppURL != "" {
		token = managedSecret
	}

	var (
		client  llm.Client
		initErr error
	)
	switch settings.LLMMode {
	case LLMModeMock:
		// Explicit dev/test-only mock client; never silently activated when LLMMode == grafana-llm-app.
		client = &llm.MockClient{
			Delay: time.Duration(settings.MockDelayMs) * time.Millisecond,
		}
	case LLMModeGrafanaLLMApp:
		client, initErr = llm.NewGrafanaLLMClient(llm.GrafanaLLMConfig{
			GrafanaURL:           settings.GrafanaURL,
			ServiceAccountToken:  token,
			ModelTier:            llmclient.Model(settings.ModelTier),
			StructuredOutputMode: settings.StructuredOutputMode,
			TokenLimitField:      settings.TokenLimitField,
			MaxOutputTokens:      settings.MaxOutputTokens,
			ReasoningEffort:      settings.EffectiveReasoningEffort,
			RequestTimeout:       settings.LLMTimeout(),
			MaxTransportRetries:  1,
		})
	default:
		initErr = fmt.Errorf("unsupported llmMode: %s", settings.LLMMode)
	}

	var (
		store        jobs.JobStore
		storeInitErr error
	)
	switch settings.JobStoreBackend {
	case "", JobStoreBackendMemory:
		store = jobs.NewMemoryStore()
	case JobStoreBackendRedis:
		// Never silently fall back to single-process memory when Redis is explicitly configured for HA.
		store, storeInitErr = jobs.NewRedisStore(jobs.RedisConfig{
			Addr:      settings.RedisAddr,
			Username:  settings.RedisUsername,
			Password:  settings.redisPassword,
			DB:        settings.RedisDB,
			KeyPrefix: settings.RedisKeyPrefix,
			TLS:       settings.RedisTLS,
		})
	case JobStoreBackendPostgres:
		// Never silently fall back to single-process memory when PostgreSQL is explicitly configured for HA.
		store, storeInitErr = jobs.NewPostgresStore(jobs.PostgresConfig{
			Addr:        settings.PostgresAddr,
			Database:    settings.PostgresDatabase,
			Username:    settings.PostgresUsername,
			Password:    settings.postgresPassword,
			Schema:      settings.PostgresSchema,
			TablePrefix: settings.PostgresTablePrefix,
			SSLMode:     settings.PostgresSSLMode,
		})
	default:
		storeInitErr = fmt.Errorf("unsupported jobStoreBackend: %s", settings.JobStoreBackend)
	}

	return newAppWithClientAndStore(settings, client, initErr, store, storeInitErr), nil
}

func newAppWithClient(settings Settings, client llm.Client, initErr error) *App {
	return newAppWithClientAndStore(settings, client, initErr, jobs.NewMemoryStore(), nil)
}

func newAppWithClientAndStore(settings Settings, client llm.Client, initErr error, store jobs.JobStore, storeInitErr error) *App {
	var mgr *jobs.Manager
	if client != nil && store != nil && storeInitErr == nil {
		mgr = jobs.NewManager(client, jobs.Config{
			MaxPanels:            settings.MaxPanels,
			MaxRepairAttempts:    settings.EffectiveMaxRepairs,
			MaxRepairAttemptsSet: true,
			JobDeadline:          settings.JobDeadline(),
			JobTTL:               settings.JobTTL(),
			MaxConcurrentJobs:    settings.MaxConcurrentJobs,
			AllowedDSUIDs:        settings.AllowedDatasourceUIDs,
			DatasourceDialects:   settings.DatasourceDialects,
			Store:                store,
		})
	}
	a := &App{
		settings:             settings,
		llmClient:            client,
		llmInitErr:           initErr,
		jobStore:             store,
		storeInitErr:         storeInitErr,
		jobManager:           mgr,
		sseHeartbeatInterval: defaultSSEHeartbeatInterval,
	}
	mux := http.NewServeMux()
	a.registerRoutes(mux)
	a.resourceHandler = httpadapter.New(mux)
	return a
}

// Dispose cancels all active in-flight jobs and closes the job store when Grafana recycles the plugin instance.
func (a *App) Dispose() {
	if a.jobManager != nil {
		a.jobManager.Close()
	}
	if a.jobStore != nil {
		_ = a.jobStore.Close()
	}
}

// CallResource delegates plugin resource HTTP requests to the internal ServeMux.
func (a *App) CallResource(ctx context.Context, req *backend.CallResourceRequest, sender backend.CallResourceResponseSender) error {
	return a.resourceHandler.CallResource(ctx, req, sender)
}

const healthPingTimeout = 3 * time.Second

// CheckHealth verifies configuration, job store connectivity, and connectivity to grafana-llm-app (or explicit dev mock).
func (a *App) CheckHealth(ctx context.Context, _ *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	if a.llmInitErr != nil || a.llmClient == nil {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: fmt.Sprintf("LLM client not initialized (%s): %v", a.settings.LLMMode, a.llmInitErr),
		}, nil
	}
	if a.storeInitErr != nil || a.jobStore == nil {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: fmt.Sprintf("Job store not initialized (%s): %v", a.settings.JobStoreBackend, a.storeInitErr),
		}, nil
	}
	pingCtx, pingCancel := context.WithTimeout(ctx, healthPingTimeout)
	defer pingCancel()
	if err := a.jobStore.Ping(pingCtx); err != nil {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: fmt.Sprintf("Job store health check failed (%s): %v", a.settings.JobStoreBackend, err),
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
	storeBackend := a.settings.JobStoreBackend
	if storeBackend == "" {
		storeBackend = JobStoreBackendMemory
	}
	return &backend.CheckHealthResult{
		Status:  backend.HealthStatusOk,
		Message: fmt.Sprintf("LLM Dashboard Studio ready (mode=%s, modelTier=%s, jobStore=%s, allowedDatasources=%d)", a.settings.LLMMode, a.settings.ModelTier, storeBackend, len(a.settings.AllowedDatasourceUIDs)),
	}, nil
}

func (a *App) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/health", a.handleHealth)
	mux.HandleFunc("/config/public", a.handlePublicConfig)
	mux.HandleFunc("/jobs/generate", a.handleGenerate)
	mux.HandleFunc("/jobs/analyze", a.handleAnalyzeStatus)
	mux.HandleFunc("/jobs/import", a.handleImport)
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
	if a.llmInitErr != nil || a.llmClient == nil || a.storeInitErr != nil || a.jobStore == nil || len(a.settings.AllowedDatasourceUIDs) == 0 {
		msg := "service not ready"
		if isAdmin {
			if a.llmInitErr != nil {
				msg = a.llmInitErr.Error()
			} else if a.storeInitErr != nil {
				msg = a.storeInitErr.Error()
			}
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status":  "ERROR",
			"message": msg,
		})
		return
	}
	// Avoid triggering live LLM test completions on non-admin /health probes;
	// verify job store reachability without exposing internal details.
	if !isAdmin {
		pingCtx, pingCancel := context.WithTimeout(r.Context(), healthPingTimeout)
		defer pingCancel()
		if err := a.jobStore.Ping(pingCtx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status":  "ERROR",
				"message": "service not ready",
			})
			return
		}
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
	owner, err := callerOwnerFromContext(r.Context())
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if a.jobManager == nil {
		writeError(w, http.StatusServiceUnavailable, "LLM client or job store unavailable; check plugin configuration")
		return
	}

	var in jobs.GenerateInput
	if err := decodeBoundedJSON(r.Body, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	a.serveJobOperation(w, r, func(ctx context.Context) (jobs.JobSnapshot, error) {
		return a.jobManager.Generate(ctx, owner, in)
	})
}

func (a *App) handleAnalyzeStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	owner, err := callerOwnerFromContext(r.Context())
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if a.jobManager == nil {
		writeError(w, http.StatusServiceUnavailable, "LLM client or job store unavailable; check plugin configuration")
		return
	}

	var in ir.AnalyzeStatusPayload
	if err := decodeBoundedJSON(r.Body, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	a.serveJobOperation(w, r, func(ctx context.Context) (jobs.JobSnapshot, error) {
		return a.jobManager.AnalyzeStatus(ctx, owner, in)
	})
}

type importHTTPRequest struct {
	DatasourceUID string                 `json:"datasourceUid"`
	QueryKind     string                 `json:"queryKind,omitempty"`
	Scope         *ir.ScopeFilter        `json:"scope,omitempty"`
	Draft         json.RawMessage        `json:"draft"`
	ImportSource  *ir.ImportSource       `json:"importSource,omitempty"`
	Catalog       []ir.MetricCatalogItem `json:"catalog,omitempty"`
}

func (a *App) handleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	owner, err := callerOwnerFromContext(r.Context())
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if a.jobManager == nil {
		writeError(w, http.StatusServiceUnavailable, "LLM client or job store unavailable; check plugin configuration")
		return
	}

	var req importHTTPRequest
	if err := decodeBoundedJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	draft, err := ir.DecodeStrictIR(req.Draft)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid draft IR: %v", err))
		return
	}

	snap, err := a.jobManager.ImportContext(r.Context(), owner, jobs.ImportInput{
		DatasourceUID: req.DatasourceUID,
		QueryKind:     req.QueryKind,
		Scope:         req.Scope,
		Draft:         draft,
		ImportSource:  req.ImportSource,
		Catalog:       req.Catalog,
	})
	if err != nil {
		writeJobError(w, err, snap)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (a *App) handleJobAction(w http.ResponseWriter, r *http.Request) {
	owner, err := callerOwnerFromContext(r.Context())
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if a.jobManager == nil {
		writeError(w, http.StatusServiceUnavailable, "LLM client or job store unavailable; check plugin configuration")
		return
	}

	trimmed := strings.TrimPrefix(r.URL.Path, "/jobs/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) == 0 || len(parts) > 2 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "invalid job route")
		return
	}
	jobID := parts[0]
	if err := jobs.ValidateJobID(jobID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch {
	case action == "" && r.Method == http.MethodGet:
		snap, err := a.jobManager.GetContext(r.Context(), owner, jobID)
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
		a.serveJobOperation(w, r, func(ctx context.Context) (jobs.JobSnapshot, error) {
			return a.jobManager.Patch(ctx, owner, jobID, in)
		})

	case action == "repair" && r.Method == http.MethodPost:
		var in jobs.RepairInput
		if err := decodeBoundedJSON(r.Body, &in); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.serveJobOperation(w, r, func(ctx context.Context) (jobs.JobSnapshot, error) {
			return a.jobManager.Repair(ctx, owner, jobID, in)
		})

	case action == "undo" && r.Method == http.MethodPost:
		snap, err := a.jobManager.UndoContext(r.Context(), owner, jobID)
		if err != nil {
			writeJobError(w, err, snap)
			return
		}
		writeJSON(w, http.StatusOK, snap)

	case action == "cancel" && r.Method == http.MethodPost:
		snap, err := a.jobManager.CancelContext(r.Context(), owner, jobID)
		if err != nil {
			writeJobError(w, err, snap)
			return
		}
		writeJSON(w, http.StatusOK, snap)

	default:
		writeError(w, http.StatusNotFound, "unknown job route or method")
	}
}

// serveJobOperation executes a generate/patch/repair job operation, emitting decoupled
// application-level SSE stage-progress events when both admin enableSseProgress=true
// and the caller requests Accept: text/event-stream (with graceful JSON fallback otherwise).
func (a *App) serveJobOperation(w http.ResponseWriter, r *http.Request, op func(ctx context.Context) (jobs.JobSnapshot, error)) {
	if a.settings.EnableSSEProgress && wantsEventStream(r) {
		if flusher, ok := w.(http.Flusher); ok {
			sw := newSSEStreamWriter(r.Context(), w, flusher, a.sseHeartbeatInterval)
			defer sw.StopHeartbeat()

			opCtx := jobs.WithStageObserver(r.Context(), sw.EmitStage)
			snap, err := op(opCtx)
			if err != nil {
				sw.FinishError(err, snap)
				return
			}
			sw.FinishResult(snap)
			return
		}
	}

	if a.settings.EnableSSEProgress {
		w.Header().Set("Vary", "Accept")
	}
	snap, err := op(r.Context())
	if err != nil {
		writeJobErrorForContext(r.Context(), w, err, snap)
		return
	}
	writeJSON(w, http.StatusOK, snap)
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

func buildJobErrorPayload(err error, snap jobs.JobSnapshot) (int, map[string]any) {
	return buildJobErrorPayloadForContext(context.Background(), err, snap)
}

func buildJobErrorPayloadForContext(ctx context.Context, err error, snap jobs.JobSnapshot) (int, map[string]any) {
	status := http.StatusBadRequest
	errMsg := err.Error()
	pCtx := backend.PluginConfigFromContext(ctx)
	isAdmin := pCtx.User != nil && strings.EqualFold(pCtx.User.Role, "Admin")

	switch {
	case errors.Is(err, jobs.ErrJobNotFound):
		status = http.StatusNotFound
	case errors.Is(err, jobs.ErrForbidden), errors.Is(err, jobs.ErrDatasourceDenied):
		status = http.StatusForbidden
	case errors.Is(err, ir.ErrRevisionConflict), errors.Is(err, jobs.ErrOperationInProgress), errors.Is(err, jobs.ErrJobCancelled), errors.Is(err, jobs.ErrWrongJobKind):
		status = http.StatusConflict
	case errors.Is(err, jobs.ErrBudgetExhausted), errors.Is(err, jobs.ErrConcurrencyLimited):
		status = http.StatusTooManyRequests
	case errors.Is(err, jobs.ErrNotRepairable), errors.Is(err, jobs.ErrNoUndoHistory), errors.Is(err, ir.ErrNoOpPatch), errors.Is(err, ir.ErrInsufficientEvidence):
		status = http.StatusUnprocessableEntity
	case errors.Is(err, jobs.ErrReportStorageMigrationRequired):
		status = http.StatusServiceUnavailable
		if !isAdmin {
			errMsg = jobs.ErrReportStorageMigrationRequired.Error()
		}
	case errors.Is(err, jobs.ErrStoreUnavailable):
		status = http.StatusServiceUnavailable
		errMsg = jobs.ErrStoreUnavailable.Error()
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
	case errors.Is(err, ir.ErrInvalidReportIR), errors.Is(err, llm.ErrOutputTruncated), errors.Is(err, llm.ErrOutputRefused), errors.Is(err, llm.ErrLLMUnavailable):
		status = http.StatusBadGateway
	case errors.Is(err, ir.ErrInvalidAnalyzeRequest):
		status = http.StatusBadRequest
	}
	payload := map[string]any{
		"error": errMsg,
	}
	if snap.JobID != "" {
		payload["job"] = snap
	}
	return status, payload
}

func writeJobError(w http.ResponseWriter, err error, snap jobs.JobSnapshot) {
	status, payload := buildJobErrorPayload(err, snap)
	writeJSON(w, status, payload)
}

func writeJobErrorForContext(ctx context.Context, w http.ResponseWriter, err error, snap jobs.JobSnapshot) {
	status, payload := buildJobErrorPayloadForContext(ctx, err, snap)
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
