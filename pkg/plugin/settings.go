package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/grafana/grafana-llm-app/llmclient"
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
)

const (
	LLMModeGrafanaLLMApp = "grafana-llm-app"
	LLMModeMock          = "mock"
)

var allowedReasoningEfforts = map[string]bool{
	"":       true,
	"low":    true,
	"medium": true,
	"high":   true,
}

// JSONSettings holds non-secret admin configuration stored in Grafana plugin jsonData.
type JSONSettings struct {
	AllowedDatasourceUIDs []string `json:"allowedDatasourceUids"`
	LLMMode               string   `json:"llmMode"`
	AllowMockInDev        bool     `json:"allowMockInDev"`
	GrafanaURL            string   `json:"grafanaUrl"`
	ModelTier             string   `json:"modelTier"`
	ReasoningEffort       *string  `json:"reasoningEffort,omitempty"`
	MaxPanels             int      `json:"maxPanels"`
	MaxRepairAttempts     *int     `json:"maxRepairAttempts,omitempty"`
	MaxOutputTokens       int      `json:"maxOutputTokens"`
	LLMTimeoutSeconds     int      `json:"llmTimeoutSeconds"`
	JobDeadlineSeconds    int      `json:"jobDeadlineSeconds"`
	MaxConcurrentJobs     int      `json:"maxConcurrentJobs"`
	ValidationTimeoutSec  int      `json:"validationTimeoutSeconds"`
	MaxConcurrentQueries  int      `json:"maxConcurrentQueries"`
}

// Settings combines validated jsonData and decrypted secureJsonData.
type Settings struct {
	JSONSettings
	EffectiveReasoningEffort string
	EffectiveMaxRepairs      int
	// llmServiceAccountToken is strictly scoped for llmclient calls to grafana-llm-app ONLY.
	// It MUST NEVER be reused for datasource queries or dashboard persistence.
	llmServiceAccountToken string
}

// PublicConfig is the secret-free subset exposed to the Frontend via GET /config/public.
type PublicConfig struct {
	AllowedDatasourceUIDs    []string `json:"allowedDatasourceUids"`
	LLMMode                  string   `json:"llmMode"`
	ModelTier                string   `json:"modelTier"`
	MaxPanels                int      `json:"maxPanels"`
	MaxRepairAttempts        int      `json:"maxRepairAttempts"`
	ValidationTimeoutSeconds int      `json:"validationTimeoutSeconds"`
	MaxConcurrentQueries     int      `json:"maxConcurrentQueries"`
	MockBanner               bool     `json:"mockBanner"`
}

func (s Settings) Public() PublicConfig {
	ds := make([]string, len(s.AllowedDatasourceUIDs))
	copy(ds, s.AllowedDatasourceUIDs)
	return PublicConfig{
		AllowedDatasourceUIDs:    ds,
		LLMMode:                  s.LLMMode,
		ModelTier:                s.ModelTier,
		MaxPanels:                s.MaxPanels,
		MaxRepairAttempts:        s.EffectiveMaxRepairs,
		ValidationTimeoutSeconds: s.ValidationTimeoutSec,
		MaxConcurrentQueries:     s.MaxConcurrentQueries,
		MockBanner:               s.LLMMode == LLMModeMock,
	}
}

// LoadSettings validates plugin configuration against trustedGrafanaAppURL (from GrafanaConfig.AppURL)
// to prevent SSRF and SA token exfiltration via arbitrary grafanaUrl values.
func LoadSettings(appSettings backend.AppInstanceSettings, trustedGrafanaAppURL ...string) (Settings, error) {
	var js JSONSettings
	if len(appSettings.JSONData) > 0 {
		if err := json.Unmarshal(appSettings.JSONData, &js); err != nil {
			return Settings{}, fmt.Errorf("parse app jsonData: %w", err)
		}
	}

	trustedAppURL := ""
	if len(trustedGrafanaAppURL) > 0 {
		trustedAppURL = strings.TrimSpace(trustedGrafanaAppURL[0])
	}

	if strings.TrimSpace(js.LLMMode) == "" {
		js.LLMMode = LLMModeGrafanaLLMApp
	}
	if strings.TrimSpace(js.GrafanaURL) == "" {
		if trustedAppURL != "" {
			js.GrafanaURL = trustedAppURL
		} else {
			js.GrafanaURL = "http://localhost:3000"
		}
	}
	if err := validateGrafanaURL(js.GrafanaURL, trustedAppURL); err != nil {
		return Settings{}, err
	}

	if js.ModelTier != string(llmclient.ModelBase) && js.ModelTier != string(llmclient.ModelLarge) {
		js.ModelTier = string(llmclient.ModelBase)
	}

	// Default ReasoningEffort to "low" (matching docs) when omitted; allow explicit "" to unset.
	reasoning := "low"
	if js.ReasoningEffort != nil {
		reasoning = strings.TrimSpace(strings.ToLower(*js.ReasoningEffort))
	}
	if !allowedReasoningEfforts[reasoning] {
		return Settings{}, fmt.Errorf("unsupported reasoningEffort %q (allowed: \"\", low, medium, high)", reasoning)
	}

	if js.MaxPanels <= 0 || js.MaxPanels > ir.MaxPanelsHardCap {
		js.MaxPanels = ir.DefaultMaxPanels
	}

	// Support explicit maxRepairAttempts=0 to disable automatic syntax repair.
	effectiveRepairs := 2
	if js.MaxRepairAttempts != nil {
		effectiveRepairs = *js.MaxRepairAttempts
		if effectiveRepairs < 0 {
			effectiveRepairs = 0
		}
		if effectiveRepairs > 2 {
			effectiveRepairs = 2
		}
	}

	if js.MaxOutputTokens <= 0 || js.MaxOutputTokens > 8192 {
		js.MaxOutputTokens = 2048
	}
	if js.LLMTimeoutSeconds <= 0 || js.LLMTimeoutSeconds > 120 {
		js.LLMTimeoutSeconds = 45
	}
	if js.JobDeadlineSeconds <= 0 || js.JobDeadlineSeconds > 180 {
		js.JobDeadlineSeconds = 90
	}
	if js.MaxConcurrentJobs <= 0 || js.MaxConcurrentJobs > 16 {
		js.MaxConcurrentJobs = 4
	}
	if js.ValidationTimeoutSec <= 0 || js.ValidationTimeoutSec > 30 {
		js.ValidationTimeoutSec = 10
	}
	if js.MaxConcurrentQueries <= 0 || js.MaxConcurrentQueries > 4 {
		js.MaxConcurrentQueries = 2
	}

	cleaned := make([]string, 0, len(js.AllowedDatasourceUIDs))
	for _, uid := range js.AllowedDatasourceUIDs {
		u := strings.TrimSpace(uid)
		if u != "" {
			cleaned = append(cleaned, u)
		}
	}
	js.AllowedDatasourceUIDs = cleaned

	if js.LLMMode != LLMModeGrafanaLLMApp && js.LLMMode != LLMModeMock {
		return Settings{}, fmt.Errorf("unsupported llmMode %q (must be %q or %q)", js.LLMMode, LLMModeGrafanaLLMApp, LLMModeMock)
	}
	if js.LLMMode == LLMModeMock && !js.AllowMockInDev {
		return Settings{}, errors.New("llmMode=mock requires explicit allowMockInDev=true (dev/test only)")
	}

	var token string
	if appSettings.DecryptedSecureJSONData != nil {
		token = strings.TrimSpace(appSettings.DecryptedSecureJSONData["llmServiceAccountToken"])
	}

	return Settings{
		JSONSettings:             js,
		EffectiveReasoningEffort: reasoning,
		EffectiveMaxRepairs:      effectiveRepairs,
		llmServiceAccountToken:   token,
	}, nil
}

// validateGrafanaURL restricts llmclient target URLs to http/https matching either the
// trusted Grafana AppURL origin or loopback, preventing SSRF and Bearer token exfiltration.
func validateGrafanaURL(rawURL, trustedAppURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("invalid grafanaUrl: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("grafanaUrl scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" || u.User != nil {
		return errors.New("grafanaUrl must have a valid host and must not embed credentials")
	}
	if isLoopbackHost(u.Hostname()) {
		return nil
	}
	if trustedAppURL != "" {
		tu, err := url.Parse(trustedAppURL)
		if err == nil && strings.EqualFold(u.Scheme, tu.Scheme) && strings.EqualFold(u.Host, tu.Host) {
			return nil
		}
	}
	return fmt.Errorf("grafanaUrl host %q is not loopback and does not match trusted Grafana AppURL origin", u.Host)
}

func isLoopbackHost(host string) bool {
	h := strings.TrimSpace(strings.ToLower(host))
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func (s Settings) LLMTimeout() time.Duration {
	return time.Duration(s.LLMTimeoutSeconds) * time.Second
}

func (s Settings) JobDeadline() time.Duration {
	return time.Duration(s.JobDeadlineSeconds) * time.Second
}
