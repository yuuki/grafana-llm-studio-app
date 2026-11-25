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
	"github.com/yuuki/grafana-llm-studio-app/pkg/jobs"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
	"github.com/yuuki/grafana-llm-studio-app/pkg/query"
)

const (
	LLMModeGrafanaLLMApp  = "grafana-llm-app"
	LLMModeMock           = "mock"
	JobStoreBackendMemory = "memory"
	JobStoreBackendRedis  = "redis"
)

var allowedReasoningEfforts = map[string]bool{
	"":       true,
	"low":    true,
	"medium": true,
	"high":   true,
}

var allowedStructuredOutputModes = map[string]bool{
	llm.StructuredOutputJSONSchema: true,
	llm.StructuredOutputJSONObject: true,
}

var allowedTokenLimitFields = map[string]bool{
	llm.TokenLimitMaxCompletionTokens: true,
	llm.TokenLimitMaxTokens:           true,
}

// JSONSettings holds non-secret admin configuration stored in Grafana plugin jsonData.
type JSONSettings struct {
	AllowedDatasourceUIDs []string          `json:"allowedDatasourceUids"`
	DatasourceDialects    map[string]string `json:"datasourceDialects,omitempty"`
	LLMMode               string            `json:"llmMode"`
	AllowMockInDev        bool              `json:"allowMockInDev"`
	GrafanaURL            string            `json:"grafanaUrl"`
	ModelTier             string            `json:"modelTier"`
	StructuredOutputMode  string            `json:"structuredOutputMode,omitempty"`
	TokenLimitField       string            `json:"tokenLimitField,omitempty"`
	ReasoningEffort       *string           `json:"reasoningEffort,omitempty"`
	MaxPanels             int               `json:"maxPanels"`
	MaxRepairAttempts     *int              `json:"maxRepairAttempts,omitempty"`
	MaxOutputTokens       int               `json:"maxOutputTokens"`
	LLMTimeoutSeconds     int               `json:"llmTimeoutSeconds"`
	JobDeadlineSeconds    int               `json:"jobDeadlineSeconds"`
	MaxConcurrentJobs     int               `json:"maxConcurrentJobs"`
	ValidationTimeoutSec  int               `json:"validationTimeoutSeconds"`
	MaxConcurrentQueries  int               `json:"maxConcurrentQueries"`
	EnableSSEProgress     bool              `json:"enableSseProgress,omitempty"`
	MockDelayMs           int               `json:"mockDelayMs,omitempty"`
	JobStoreBackend       string            `json:"jobStoreBackend,omitempty"`
	JobTTLMinutes         int               `json:"jobTtlMinutes,omitempty"`
	RedisAddr             string            `json:"redisAddr,omitempty"`
	RedisDB               int               `json:"redisDb,omitempty"`
	RedisKeyPrefix        string            `json:"redisKeyPrefix,omitempty"`
	RedisTLS              bool              `json:"redisTls,omitempty"`
	RedisUsername         string            `json:"redisUsername,omitempty"`
}

// Settings combines validated jsonData and decrypted secureJsonData.
type Settings struct {
	JSONSettings
	EffectiveReasoningEffort string
	EffectiveMaxRepairs      int
	// llmServiceAccountToken is strictly scoped for llmclient calls to grafana-llm-app ONLY.
	// It MUST NEVER be reused for datasource queries or dashboard persistence.
	llmServiceAccountToken string
	// redisPassword is the optional decrypted secret for jobStoreBackend=redis.
	// It MUST NEVER be exposed in PublicConfig, health responses, or logs.
	redisPassword string
}

// PublicConfig is the secret-free subset exposed to the Frontend via GET /config/public.
type PublicConfig struct {
	AllowedDatasourceUIDs    []string          `json:"allowedDatasourceUids"`
	DatasourceDialects       map[string]string `json:"datasourceDialects,omitempty"`
	LLMMode                  string            `json:"llmMode"`
	ModelTier                string            `json:"modelTier"`
	MaxPanels                int               `json:"maxPanels"`
	MaxRepairAttempts        int               `json:"maxRepairAttempts"`
	ValidationTimeoutSeconds int               `json:"validationTimeoutSeconds"`
	MaxConcurrentQueries     int               `json:"maxConcurrentQueries"`
	EnableSSEProgress        bool              `json:"enableSseProgress"`
	MockBanner               bool              `json:"mockBanner"`
}

func (s Settings) Public() PublicConfig {
	ds := make([]string, len(s.AllowedDatasourceUIDs))
	copy(ds, s.AllowedDatasourceUIDs)
	var dialects map[string]string
	if len(s.DatasourceDialects) > 0 {
		allowSet := make(map[string]bool, len(ds))
		for _, uid := range ds {
			allowSet[uid] = true
		}
		for uid, kind := range s.DatasourceDialects {
			if allowSet[uid] && kind != "" {
				if dialects == nil {
					dialects = make(map[string]string)
				}
				dialects[uid] = kind
			}
		}
	}
	return PublicConfig{
		AllowedDatasourceUIDs:    ds,
		DatasourceDialects:       dialects,
		LLMMode:                  s.LLMMode,
		ModelTier:                s.ModelTier,
		MaxPanels:                s.MaxPanels,
		MaxRepairAttempts:        s.EffectiveMaxRepairs,
		ValidationTimeoutSeconds: s.ValidationTimeoutSec,
		MaxConcurrentQueries:     s.MaxConcurrentQueries,
		EnableSSEProgress:        s.EnableSSEProgress,
		MockBanner:               s.LLMMode == LLMModeMock,
	}
}

const (
	BoundRedisPasswordPrefix = "llmstudio-redis-bound-v1:"   //nolint:gosec // G101: envelope format prefix constant, not a credential
	BoundGrafanaTokenPrefix  = "llmstudio-grafana-bound-v1:" //nolint:gosec // G101: envelope format prefix constant, not a credential
)

// LoadOptions supplies deployment-level trusted boundaries from Grafana or process environment.
type LoadOptions struct {
	// TrustedGrafanaAppURL pins llmclient traffic to Grafana's trusted base URL (from LLMSTUDIO_GRAFANA_URL or GrafanaConfig.AppURL).
	TrustedGrafanaAppURL string
	// PinnedRedisAddr optionally pins redisAddr at the deployment level (from LLMSTUDIO_REDIS_ADDR).
	PinnedRedisAddr string
}

type boundRedisPasswordPayload struct {
	Addr     string `json:"addr"`
	TLS      bool   `json:"tls"`
	Password string `json:"password"`
}

type boundGrafanaTokenPayload struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// EncodeBoundRedisPassword encodes a Redis password bound to a specific Redis host:port and TLS mode.
func EncodeBoundRedisPassword(addr string, tlsEnabled bool, password string) (string, error) {
	if err := jobs.ValidateRedisAddr(addr); err != nil {
		return "", err
	}
	if password == "" {
		return "", errors.New("redis password must not be empty")
	}
	b, err := json.Marshal(boundRedisPasswordPayload{ //nolint:gosec // G117: intentionally marshals destination-bound secret envelope for encrypted secureJsonData
		Addr:     strings.TrimSpace(addr),
		TLS:      tlsEnabled,
		Password: password,
	})
	if err != nil {
		return "", err
	}
	return BoundRedisPasswordPrefix + string(b), nil
}

// EncodeBoundGrafanaToken encodes a Grafana service account token bound to a specific Grafana base URL
// (or empty string to bind to Grafana's trusted AppURL default).
func EncodeBoundGrafanaToken(grafanaURL, token string) (string, error) {
	trimmedToken := strings.TrimSpace(token)
	if trimmedToken == "" {
		return "", errors.New("grafana service account token must not be empty")
	}
	trimmedURL := strings.TrimSpace(grafanaURL)
	if trimmedURL != "" {
		if _, err := canonicalGrafanaURL(trimmedURL); err != nil {
			return "", err
		}
	}
	b, err := json.Marshal(boundGrafanaTokenPayload{
		URL:   trimmedURL,
		Token: trimmedToken,
	})
	if err != nil {
		return "", err
	}
	return BoundGrafanaTokenPrefix + string(b), nil
}

// LoadSettings validates plugin configuration against trustedGrafanaAppURL (from GrafanaConfig.AppURL)
// to prevent SSRF and SA token exfiltration via arbitrary grafanaUrl values.
func LoadSettings(appSettings backend.AppInstanceSettings, trustedGrafanaAppURL ...string) (Settings, error) {
	var opts LoadOptions
	if len(trustedGrafanaAppURL) > 0 {
		opts.TrustedGrafanaAppURL = strings.TrimSpace(trustedGrafanaAppURL[0])
	}
	return LoadSettingsWithOptions(appSettings, opts)
}

// LoadSettingsWithOptions validates plugin configuration against deployment-level trusted boundaries
// and enforces destination binding on stored credentials.
func LoadSettingsWithOptions(appSettings backend.AppInstanceSettings, opts LoadOptions) (Settings, error) {
	var js JSONSettings
	if len(appSettings.JSONData) > 0 {
		if err := json.Unmarshal(appSettings.JSONData, &js); err != nil {
			return Settings{}, fmt.Errorf("parse app jsonData: %w", err)
		}
	}

	trustedAppURL := strings.TrimSpace(opts.TrustedGrafanaAppURL)
	pinnedRedisAddr := strings.TrimSpace(opts.PinnedRedisAddr)

	if strings.TrimSpace(js.LLMMode) == "" {
		js.LLMMode = LLMModeGrafanaLLMApp
	}
	rawGrafanaURL := strings.TrimSpace(js.GrafanaURL)
	if rawGrafanaURL == "" {
		if trustedAppURL != "" {
			js.GrafanaURL = trustedAppURL
		} else {
			js.GrafanaURL = "http://localhost:3000"
		}
	} else {
		js.GrafanaURL = rawGrafanaURL
	}
	if err := validateGrafanaURL(js.GrafanaURL, trustedAppURL); err != nil {
		return Settings{}, err
	}

	if js.ModelTier != string(llmclient.ModelBase) && js.ModelTier != string(llmclient.ModelLarge) {
		js.ModelTier = string(llmclient.ModelBase)
	}

	js.StructuredOutputMode = strings.TrimSpace(strings.ToLower(js.StructuredOutputMode))
	if js.StructuredOutputMode == "" {
		js.StructuredOutputMode = llm.StructuredOutputJSONSchema
	}
	if !allowedStructuredOutputModes[js.StructuredOutputMode] {
		return Settings{}, fmt.Errorf("unsupported structuredOutputMode %q (allowed: json_schema, json_object)", js.StructuredOutputMode)
	}

	js.TokenLimitField = strings.TrimSpace(strings.ToLower(js.TokenLimitField))
	if js.TokenLimitField == "" {
		js.TokenLimitField = llm.TokenLimitMaxCompletionTokens
	}
	if !allowedTokenLimitFields[js.TokenLimitField] {
		return Settings{}, fmt.Errorf("unsupported tokenLimitField %q (allowed: max_completion_tokens, max_tokens)", js.TokenLimitField)
	}

	// Default ReasoningEffort to "" (omitted) so standard OpenAI and OpenAI-compatible models
	// (e.g., GPT-4o, GPT-4.1, Llama, DeepSeek-Chat) do not fail with HTTP 400 unsupported parameter.
	// Reasoning models (e.g., o3, GPT-5, GLM-5.3) can explicitly opt into "low", "medium", or "high".
	reasoning := ""
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

	if len(js.DatasourceDialects) > 0 {
		normalizedDialects := make(map[string]string, len(js.DatasourceDialects))
		for rawUID, rawKind := range js.DatasourceDialects {
			uid := strings.TrimSpace(rawUID)
			if uid == "" {
				continue
			}
			kStr := strings.TrimSpace(strings.ToLower(rawKind))
			if kStr == "" {
				continue
			}
			parsed, err := query.ParseKind(kStr)
			if err != nil {
				return Settings{}, fmt.Errorf("datasourceDialects[%q]: %w", uid, err)
			}
			normalizedDialects[uid] = string(parsed)
		}
		js.DatasourceDialects = normalizedDialects
	} else {
		js.DatasourceDialects = nil
	}

	if js.LLMMode != LLMModeGrafanaLLMApp && js.LLMMode != LLMModeMock {
		return Settings{}, fmt.Errorf("unsupported llmMode %q (must be %q or %q)", js.LLMMode, LLMModeGrafanaLLMApp, LLMModeMock)
	}
	if js.LLMMode == LLMModeMock && !js.AllowMockInDev {
		return Settings{}, errors.New("llmMode=mock requires explicit allowMockInDev=true (dev/test only)")
	}
	if js.LLMMode != LLMModeMock || !js.AllowMockInDev || js.MockDelayMs < 0 {
		js.MockDelayMs = 0
	} else if js.MockDelayMs > 10000 {
		js.MockDelayMs = 10000
	}

	js.JobStoreBackend = strings.TrimSpace(strings.ToLower(js.JobStoreBackend))
	if js.JobStoreBackend == "" {
		js.JobStoreBackend = JobStoreBackendMemory
	}
	if js.JobStoreBackend != JobStoreBackendMemory && js.JobStoreBackend != JobStoreBackendRedis {
		return Settings{}, fmt.Errorf("unsupported jobStoreBackend %q (must be %q or %q)", js.JobStoreBackend, JobStoreBackendMemory, JobStoreBackendRedis)
	}
	if js.JobTTLMinutes <= 0 || js.JobTTLMinutes > 1440 {
		js.JobTTLMinutes = 30
	}
	js.RedisAddr = strings.TrimSpace(js.RedisAddr)
	js.RedisKeyPrefix = strings.TrimSpace(js.RedisKeyPrefix)
	js.RedisUsername = strings.TrimSpace(js.RedisUsername)
	if js.JobStoreBackend == JobStoreBackendRedis {
		if pinnedRedisAddr != "" {
			if err := jobs.ValidateRedisAddr(pinnedRedisAddr); err != nil {
				return Settings{}, fmt.Errorf("invalid operator-pinned LLMSTUDIO_REDIS_ADDR: %w", err)
			}
			if js.RedisAddr == "" {
				js.RedisAddr = pinnedRedisAddr
			} else if canonicalRedisAddr(js.RedisAddr) != canonicalRedisAddr(pinnedRedisAddr) {
				return Settings{}, fmt.Errorf("redisAddr %q does not match operator-pinned LLMSTUDIO_REDIS_ADDR", js.RedisAddr)
			}
		}
		if len(js.RedisUsername) > 128 {
			return Settings{}, errors.New("redisUsername exceeds maximum length of 128 characters")
		}
		if err := jobs.ValidateRedisAddr(js.RedisAddr); err != nil {
			return Settings{}, err
		}
		if js.RedisDB < 0 || js.RedisDB > 15 {
			return Settings{}, fmt.Errorf("redisDb must be between 0 and 15, got %d", js.RedisDB)
		}
		if err := jobs.ValidateRedisKeyPrefix(js.RedisKeyPrefix); err != nil {
			return Settings{}, err
		}
		if js.RedisKeyPrefix == "" {
			js.RedisKeyPrefix = jobs.DefaultRedisKeyPrefix
		}
	}

	var token string
	var redisPass string
	if appSettings.DecryptedSecureJSONData != nil {
		token = strings.TrimSpace(appSettings.DecryptedSecureJSONData["llmServiceAccountToken"])
		redisPass = appSettings.DecryptedSecureJSONData["redisPassword"]
	}

	if token != "" {
		unwrappedToken, err := unwrapBoundGrafanaToken(token, js.GrafanaURL, rawGrafanaURL, trustedAppURL)
		if err != nil {
			return Settings{}, err
		}
		token = unwrappedToken
	}

	if js.JobStoreBackend == JobStoreBackendRedis {
		if redisPass != "" {
			unwrappedPass, err := unwrapBoundRedisPassword(redisPass, js.RedisAddr, js.RedisTLS)
			if err != nil {
				return Settings{}, err
			}
			redisPass = unwrappedPass
		}
	} else {
		redisPass = ""
	}

	return Settings{
		JSONSettings:             js,
		EffectiveReasoningEffort: reasoning,
		EffectiveMaxRepairs:      effectiveRepairs,
		llmServiceAccountToken:   token,
		redisPassword:            redisPass,
	}, nil
}

func unwrapBoundRedisPassword(rawSecret, activeAddr string, activeTLS bool) (string, error) {
	if !strings.HasPrefix(rawSecret, BoundRedisPasswordPrefix) {
		return rawSecret, nil
	}
	var payload boundRedisPasswordPayload
	if err := json.Unmarshal([]byte(strings.TrimPrefix(rawSecret, BoundRedisPasswordPrefix)), &payload); err != nil {
		return "", errors.New("stored redisPassword binding metadata is malformed; re-enter or reset redisPassword")
	}
	if err := jobs.ValidateRedisAddr(payload.Addr); err != nil || payload.Password == "" {
		return "", errors.New("stored redisPassword binding metadata is incomplete; re-enter or reset redisPassword")
	}
	if canonicalRedisAddr(payload.Addr) != canonicalRedisAddr(activeAddr) || payload.TLS != activeTLS {
		return "", errors.New("stored redisPassword is bound to a different Redis endpoint or TLS setting; re-enter or reset redisPassword")
	}
	return payload.Password, nil
}

func unwrapBoundGrafanaToken(rawSecret, activeURL, rawConfiguredURL, trustedAppURL string) (string, error) {
	if !strings.HasPrefix(rawSecret, BoundGrafanaTokenPrefix) {
		return rawSecret, nil
	}
	var payload boundGrafanaTokenPayload
	if err := json.Unmarshal([]byte(strings.TrimPrefix(rawSecret, BoundGrafanaTokenPrefix)), &payload); err != nil {
		return "", errors.New("stored llmServiceAccountToken binding metadata is malformed; re-enter or reset llmServiceAccountToken")
	}
	token := strings.TrimSpace(payload.Token)
	if token == "" {
		return "", errors.New("stored llmServiceAccountToken binding metadata is incomplete; re-enter or reset llmServiceAccountToken")
	}
	boundURL := strings.TrimSpace(payload.URL)
	if boundURL == "" {
		if trustedAppURL != "" {
			// validateGrafanaURL already verified activeURL matches trustedAppURL.
			return token, nil
		}
		if rawConfiguredURL == "" {
			return token, nil
		}
		activeCanon, err := canonicalGrafanaURL(activeURL)
		defaultCanon, defErr := canonicalGrafanaURL("http://localhost:3000")
		if err != nil || defErr != nil || activeCanon != defaultCanon {
			return "", errors.New("stored llmServiceAccountToken is bound to the default Grafana URL; re-enter or reset llmServiceAccountToken")
		}
		return token, nil
	}
	boundCanon, err := canonicalGrafanaURL(boundURL)
	if err != nil {
		return "", errors.New("stored llmServiceAccountToken binding URL is invalid; re-enter or reset llmServiceAccountToken")
	}
	activeCanon, err := canonicalGrafanaURL(activeURL)
	if err != nil || boundCanon != activeCanon {
		return "", errors.New("stored llmServiceAccountToken is bound to a different Grafana URL; re-enter or reset llmServiceAccountToken")
	}
	return token, nil
}

// validateGrafanaURL restricts llmclient target URLs to http/https:
//   - When trustedAppURL is configured (from LLMSTUDIO_GRAFANA_URL or GrafanaConfig.AppURL),
//     rawURL MUST strictly match that trusted base URL (scheme, canonical host:port, and normalized path).
//     Arbitrary loopback hosts/ports are NOT allowed when trustedAppURL is present.
//   - Only when trustedAppURL is absent (standalone/test context) does it fall back to loopback-only.
func validateGrafanaURL(rawURL, trustedAppURL string) error {
	targetCanon, err := canonicalGrafanaURL(rawURL)
	if err != nil {
		return err
	}
	if strings.TrimSpace(trustedAppURL) != "" {
		trustedCanon, err := canonicalGrafanaURL(trustedAppURL)
		if err != nil {
			return fmt.Errorf("invalid trusted Grafana AppURL: %w", err)
		}
		if targetCanon != trustedCanon {
			return fmt.Errorf("grafanaUrl %q does not match trusted Grafana base URL", strings.TrimSpace(rawURL))
		}
		return nil
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("invalid grafanaUrl: %w", err)
	}
	if isLoopbackHost(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("grafanaUrl host %q is not loopback and no trusted Grafana AppURL is configured", u.Host)
}

func canonicalGrafanaURL(rawURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("invalid grafanaUrl: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("grafanaUrl scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" || u.User != nil {
		return "", errors.New("grafanaUrl must have a valid host and must not embed credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("grafanaUrl must not contain query parameters or fragments")
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "" {
		return "", errors.New("grafanaUrl must have a valid host")
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	cleanPath := strings.TrimRight(u.EscapedPath(), "/")
	if strings.Contains(cleanPath, "..") {
		return "", errors.New("grafanaUrl path must not contain path traversal segments")
	}
	return scheme + "://" + net.JoinHostPort(host, port) + cleanPath, nil
}

func canonicalRedisAddr(addr string) string {
	trimmed := strings.TrimSpace(addr)
	host, port, err := net.SplitHostPort(trimmed)
	if err != nil {
		return strings.ToLower(trimmed)
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	return net.JoinHostPort(host, port)
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

func (s Settings) JobTTL() time.Duration {
	if s.JobTTLMinutes <= 0 {
		return 30 * time.Minute
	}
	return time.Duration(s.JobTTLMinutes) * time.Minute
}
