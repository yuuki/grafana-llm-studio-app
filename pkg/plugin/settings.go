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
	LLMModeGrafanaLLMApp    = "grafana-llm-app"
	LLMModeMock             = "mock"
	JobStoreBackendMemory   = "memory"
	JobStoreBackendRedis    = "redis"
	JobStoreBackendPostgres = "postgres"
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
	PostgresAddr          string            `json:"postgresAddr,omitempty"`
	PostgresDatabase      string            `json:"postgresDatabase,omitempty"`
	PostgresSchema        string            `json:"postgresSchema,omitempty"`
	PostgresTablePrefix   string            `json:"postgresTablePrefix,omitempty"`
	PostgresUsername      string            `json:"postgresUsername,omitempty"`
	PostgresSSLMode       string            `json:"postgresSslMode,omitempty"`
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
	// postgresPassword is the optional decrypted secret for jobStoreBackend=postgres.
	// It MUST NEVER be exposed in PublicConfig, health responses, or logs.
	postgresPassword string
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
	BoundRedisPasswordPrefix    = "llmstudio-redis-bound-v1:"    //nolint:gosec // G101: envelope format prefix constant, not a credential
	BoundPostgresPasswordPrefix = "llmstudio-postgres-bound-v1:" //nolint:gosec // G101: envelope format prefix constant, not a credential
	BoundGrafanaTokenPrefix     = "llmstudio-grafana-bound-v1:"  //nolint:gosec // G101: envelope format prefix constant, not a credential
)

// LoadOptions supplies deployment-level trusted boundaries from Grafana or process environment.
type LoadOptions struct {
	// TrustedGrafanaAppURL pins llmclient traffic to Grafana's trusted base URL (from LLMSTUDIO_GRAFANA_URL or GrafanaConfig.AppURL).
	TrustedGrafanaAppURL string
	// PinnedRedisAddr optionally pins redisAddr at the deployment level (from LLMSTUDIO_REDIS_ADDR).
	PinnedRedisAddr string
	// PinnedPostgresAddr optionally pins postgresAddr at the deployment level (from LLMSTUDIO_POSTGRES_ADDR).
	PinnedPostgresAddr string
}

type boundRedisPasswordPayload struct {
	Addr     string `json:"addr"`
	TLS      bool   `json:"tls"`
	Password string `json:"password"`
}

type boundPostgresPasswordPayload struct {
	Addr     string `json:"addr"`
	SSLMode  string `json:"sslMode"`
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

// EncodeBoundPostgresPassword encodes a PostgreSQL password bound to a specific host:port and SSL mode.
func EncodeBoundPostgresPassword(addr, sslMode, password string) (string, error) {
	if err := jobs.ValidatePostgresAddr(addr); err != nil {
		return "", err
	}
	normalizedSSL := strings.TrimSpace(strings.ToLower(sslMode))
	if normalizedSSL == "" {
		normalizedSSL = jobs.DefaultPostgresSSLMode
	}
	if err := jobs.ValidatePostgresSSLMode(normalizedSSL); err != nil {
		return "", err
	}
	if password == "" {
		return "", errors.New("postgres password must not be empty")
	}
	b, err := json.Marshal(boundPostgresPasswordPayload{ //nolint:gosec // G117: intentionally marshals destination-bound secret envelope for encrypted secureJsonData
		Addr:     strings.TrimSpace(addr),
		SSLMode:  normalizedSSL,
		Password: password,
	})
	if err != nil {
		return "", err
	}
	return BoundPostgresPasswordPrefix + string(b), nil
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
	rawGrafanaURL, reasoning, effectiveRepairs, err := normalizeLLMSettings(&js, trustedAppURL)
	if err != nil {
		return Settings{}, err
	}
	normalizeNumericLimits(&js)
	if err := normalizeDatasourceSettings(&js); err != nil {
		return Settings{}, err
	}
	if err := normalizeJobStoreSettings(&js, strings.TrimSpace(opts.PinnedRedisAddr), strings.TrimSpace(opts.PinnedPostgresAddr)); err != nil {
		return Settings{}, err
	}

	token, redisPass, postgresPass, err := unwrapSettingsSecrets(appSettings.DecryptedSecureJSONData, js, rawGrafanaURL, trustedAppURL)
	if err != nil {
		return Settings{}, err
	}

	return Settings{
		JSONSettings:             js,
		EffectiveReasoningEffort: reasoning,
		EffectiveMaxRepairs:      effectiveRepairs,
		llmServiceAccountToken:   token,
		redisPassword:            redisPass,
		postgresPassword:         postgresPass,
	}, nil
}

func normalizeLLMSettings(js *JSONSettings, trustedAppURL string) (string, string, int, error) {
	if strings.TrimSpace(js.LLMMode) == "" {
		js.LLMMode = LLMModeGrafanaLLMApp
	}
	rawGrafanaURL := strings.TrimSpace(js.GrafanaURL)
	switch {
	case rawGrafanaURL != "":
		js.GrafanaURL = rawGrafanaURL
	case trustedAppURL != "":
		js.GrafanaURL = trustedAppURL
	default:
		js.GrafanaURL = "http://localhost:3000"
	}
	if err := validateGrafanaURL(js.GrafanaURL, trustedAppURL); err != nil {
		return "", "", 0, err
	}

	if js.ModelTier != string(llmclient.ModelBase) && js.ModelTier != string(llmclient.ModelLarge) {
		js.ModelTier = string(llmclient.ModelBase)
	}

	js.StructuredOutputMode = strings.TrimSpace(strings.ToLower(js.StructuredOutputMode))
	if js.StructuredOutputMode == "" {
		js.StructuredOutputMode = llm.StructuredOutputJSONSchema
	}
	if !allowedStructuredOutputModes[js.StructuredOutputMode] {
		return "", "", 0, fmt.Errorf("unsupported structuredOutputMode %q (allowed: json_schema, json_object)", js.StructuredOutputMode)
	}

	js.TokenLimitField = strings.TrimSpace(strings.ToLower(js.TokenLimitField))
	if js.TokenLimitField == "" {
		js.TokenLimitField = llm.TokenLimitMaxCompletionTokens
	}
	if !allowedTokenLimitFields[js.TokenLimitField] {
		return "", "", 0, fmt.Errorf("unsupported tokenLimitField %q (allowed: max_completion_tokens, max_tokens)", js.TokenLimitField)
	}

	// Default ReasoningEffort to "" (omitted) so standard OpenAI and OpenAI-compatible models
	// (e.g., GPT-4o, GPT-4.1, Llama, DeepSeek-Chat) do not fail with HTTP 400 unsupported parameter.
	// Reasoning models (e.g., o3, GPT-5, GLM-5.3) can explicitly opt into "low", "medium", or "high".
	reasoning := ""
	if js.ReasoningEffort != nil {
		reasoning = strings.TrimSpace(strings.ToLower(*js.ReasoningEffort))
	}
	if !allowedReasoningEfforts[reasoning] {
		return "", "", 0, fmt.Errorf("unsupported reasoningEffort %q (allowed: \"\", low, medium, high)", reasoning)
	}

	// Support explicit maxRepairAttempts=0 to disable automatic syntax repair.
	effectiveRepairs := 2
	if js.MaxRepairAttempts != nil {
		effectiveRepairs = min(max(*js.MaxRepairAttempts, 0), 2)
	}

	if err := validateLLMModeSettings(js); err != nil {
		return "", "", 0, err
	}
	return rawGrafanaURL, reasoning, effectiveRepairs, nil
}

func validateLLMModeSettings(js *JSONSettings) error {
	if js.LLMMode != LLMModeGrafanaLLMApp && js.LLMMode != LLMModeMock {
		return fmt.Errorf("unsupported llmMode %q (must be %q or %q)", js.LLMMode, LLMModeGrafanaLLMApp, LLMModeMock)
	}
	if js.LLMMode == LLMModeMock && !js.AllowMockInDev {
		return errors.New("llmMode=mock requires explicit allowMockInDev=true (dev/test only)")
	}
	if js.LLMMode != LLMModeMock || !js.AllowMockInDev || js.MockDelayMs < 0 {
		js.MockDelayMs = 0
	} else if js.MockDelayMs > 10000 {
		js.MockDelayMs = 10000
	}
	return nil
}

func normalizeNumericLimits(js *JSONSettings) {
	js.MaxPanels = defaultIfOutOfRange(js.MaxPanels, 1, ir.MaxPanelsHardCap, ir.DefaultMaxPanels)
	js.MaxOutputTokens = defaultIfOutOfRange(js.MaxOutputTokens, 1, 8192, 2048)
	js.LLMTimeoutSeconds = defaultIfOutOfRange(js.LLMTimeoutSeconds, 1, 120, 45)
	js.JobDeadlineSeconds = defaultIfOutOfRange(js.JobDeadlineSeconds, 1, 180, 90)
	js.MaxConcurrentJobs = defaultIfOutOfRange(js.MaxConcurrentJobs, 1, 16, 4)
	js.ValidationTimeoutSec = defaultIfOutOfRange(js.ValidationTimeoutSec, 1, 30, 10)
	js.MaxConcurrentQueries = defaultIfOutOfRange(js.MaxConcurrentQueries, 1, 4, 2)
}

func defaultIfOutOfRange(val, minVal, maxVal, def int) int {
	if val < minVal || val > maxVal {
		return def
	}
	return val
}

func normalizeDatasourceSettings(js *JSONSettings) error {
	cleaned := make([]string, 0, len(js.AllowedDatasourceUIDs))
	for _, uid := range js.AllowedDatasourceUIDs {
		if u := strings.TrimSpace(uid); u != "" {
			cleaned = append(cleaned, u)
		}
	}
	js.AllowedDatasourceUIDs = cleaned

	if len(js.DatasourceDialects) == 0 {
		js.DatasourceDialects = nil
		return nil
	}
	normalizedDialects := make(map[string]string, len(js.DatasourceDialects))
	for rawUID, rawKind := range js.DatasourceDialects {
		uid := strings.TrimSpace(rawUID)
		kStr := strings.TrimSpace(strings.ToLower(rawKind))
		if uid == "" || kStr == "" {
			continue
		}
		parsed, err := query.ParseKind(kStr)
		if err != nil {
			return fmt.Errorf("datasourceDialects[%q]: %w", uid, err)
		}
		normalizedDialects[uid] = string(parsed)
	}
	js.DatasourceDialects = normalizedDialects
	return nil
}

func normalizeJobStoreSettings(js *JSONSettings, pinnedRedisAddr, pinnedPostgresAddr string) error {
	js.JobStoreBackend = strings.TrimSpace(strings.ToLower(js.JobStoreBackend))
	if js.JobStoreBackend == "" {
		js.JobStoreBackend = JobStoreBackendMemory
	}
	if js.JobStoreBackend != JobStoreBackendMemory && js.JobStoreBackend != JobStoreBackendRedis && js.JobStoreBackend != JobStoreBackendPostgres {
		return fmt.Errorf("unsupported jobStoreBackend %q (must be %q, %q, or %q)", js.JobStoreBackend, JobStoreBackendMemory, JobStoreBackendRedis, JobStoreBackendPostgres)
	}
	js.JobTTLMinutes = defaultIfOutOfRange(js.JobTTLMinutes, 1, 1440, 30)

	js.RedisAddr = strings.TrimSpace(js.RedisAddr)
	js.RedisKeyPrefix = strings.TrimSpace(js.RedisKeyPrefix)
	js.RedisUsername = strings.TrimSpace(js.RedisUsername)
	if js.JobStoreBackend == JobStoreBackendRedis {
		if err := validateRedisSettings(js, pinnedRedisAddr); err != nil {
			return err
		}
	}

	js.PostgresAddr = strings.TrimSpace(js.PostgresAddr)
	js.PostgresDatabase = strings.TrimSpace(js.PostgresDatabase)
	js.PostgresSchema = strings.TrimSpace(js.PostgresSchema)
	js.PostgresTablePrefix = strings.TrimSpace(js.PostgresTablePrefix)
	js.PostgresUsername = strings.TrimSpace(js.PostgresUsername)
	js.PostgresSSLMode = strings.TrimSpace(strings.ToLower(js.PostgresSSLMode))
	if js.JobStoreBackend == JobStoreBackendPostgres {
		if err := validatePostgresSettings(js, pinnedPostgresAddr); err != nil {
			return err
		}
	}
	return nil
}

func validateRedisSettings(js *JSONSettings, pinnedRedisAddr string) error {
	if pinnedRedisAddr != "" {
		if err := jobs.ValidateRedisAddr(pinnedRedisAddr); err != nil {
			return fmt.Errorf("invalid operator-pinned LLMSTUDIO_REDIS_ADDR: %w", err)
		}
		if js.RedisAddr == "" {
			js.RedisAddr = pinnedRedisAddr
		} else if canonicalRedisAddr(js.RedisAddr) != canonicalRedisAddr(pinnedRedisAddr) {
			return fmt.Errorf("redisAddr %q does not match operator-pinned LLMSTUDIO_REDIS_ADDR", js.RedisAddr)
		}
	}
	if len(js.RedisUsername) > 128 {
		return errors.New("redisUsername exceeds maximum length of 128 characters")
	}
	if err := jobs.ValidateRedisAddr(js.RedisAddr); err != nil {
		return err
	}
	if js.RedisDB < 0 || js.RedisDB > 15 {
		return fmt.Errorf("redisDb must be between 0 and 15, got %d", js.RedisDB)
	}
	if err := jobs.ValidateRedisKeyPrefix(js.RedisKeyPrefix); err != nil {
		return err
	}
	if js.RedisKeyPrefix == "" {
		js.RedisKeyPrefix = jobs.DefaultRedisKeyPrefix
	}
	return nil
}

func validatePostgresSettings(js *JSONSettings, pinnedPostgresAddr string) error {
	if pinnedPostgresAddr != "" {
		if err := jobs.ValidatePostgresAddr(pinnedPostgresAddr); err != nil {
			return fmt.Errorf("invalid operator-pinned LLMSTUDIO_POSTGRES_ADDR: %w", err)
		}
		if js.PostgresAddr == "" {
			js.PostgresAddr = pinnedPostgresAddr
		} else if canonicalRedisAddr(js.PostgresAddr) != canonicalRedisAddr(pinnedPostgresAddr) {
			return fmt.Errorf("postgresAddr %q does not match operator-pinned LLMSTUDIO_POSTGRES_ADDR", js.PostgresAddr)
		}
	}
	if err := jobs.ValidatePostgresAddr(js.PostgresAddr); err != nil {
		return err
	}
	if err := jobs.ValidatePostgresDatabase(js.PostgresDatabase); err != nil {
		return err
	}
	if err := jobs.ValidatePostgresUsername(js.PostgresUsername); err != nil {
		return err
	}
	if err := jobs.ValidatePostgresSchema(js.PostgresSchema); err != nil {
		return err
	}
	if js.PostgresSchema == "" {
		js.PostgresSchema = jobs.DefaultPostgresSchema
	}
	if err := jobs.ValidatePostgresTablePrefix(js.PostgresTablePrefix); err != nil {
		return err
	}
	if js.PostgresTablePrefix == "" {
		js.PostgresTablePrefix = jobs.DefaultPostgresTablePrefix
	}
	if err := jobs.ValidatePostgresSSLMode(js.PostgresSSLMode); err != nil {
		return err
	}
	if js.PostgresSSLMode == "" {
		js.PostgresSSLMode = jobs.DefaultPostgresSSLMode
	}
	return nil
}

func unwrapSettingsSecrets(
	decrypted map[string]string,
	js JSONSettings,
	rawGrafanaURL, trustedAppURL string,
) (string, string, string, error) {
	if decrypted == nil {
		return "", "", "", nil
	}
	token := strings.TrimSpace(decrypted["llmServiceAccountToken"])
	redisPass := decrypted["redisPassword"]
	postgresPass := decrypted["postgresPassword"]

	if token != "" {
		unwrappedToken, err := unwrapBoundGrafanaToken(token, js.GrafanaURL, rawGrafanaURL, trustedAppURL)
		if err != nil {
			return "", "", "", err
		}
		token = unwrappedToken
	}

	if js.JobStoreBackend == JobStoreBackendRedis {
		if redisPass != "" {
			unwrappedPass, err := unwrapBoundRedisPassword(redisPass, js.RedisAddr, js.RedisTLS)
			if err != nil {
				return "", "", "", err
			}
			redisPass = unwrappedPass
		}
	} else {
		redisPass = ""
	}

	if js.JobStoreBackend == JobStoreBackendPostgres {
		if postgresPass != "" {
			unwrappedPass, err := unwrapBoundPostgresPassword(postgresPass, js.PostgresAddr, js.PostgresSSLMode)
			if err != nil {
				return "", "", "", err
			}
			postgresPass = unwrappedPass
		}
	} else {
		postgresPass = ""
	}

	return token, redisPass, postgresPass, nil
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

func unwrapBoundPostgresPassword(rawSecret, activeAddr, activeSSLMode string) (string, error) {
	if !strings.HasPrefix(rawSecret, BoundPostgresPasswordPrefix) {
		return rawSecret, nil
	}
	var payload boundPostgresPasswordPayload
	if err := json.Unmarshal([]byte(strings.TrimPrefix(rawSecret, BoundPostgresPasswordPrefix)), &payload); err != nil {
		return "", errors.New("stored postgresPassword binding metadata is malformed; re-enter or reset postgresPassword")
	}
	boundSSL := strings.TrimSpace(strings.ToLower(payload.SSLMode))
	if err := jobs.ValidatePostgresAddr(payload.Addr); err != nil || boundSSL == "" || jobs.ValidatePostgresSSLMode(boundSSL) != nil || payload.Password == "" {
		return "", errors.New("stored postgresPassword binding metadata is incomplete; re-enter or reset postgresPassword")
	}
	if canonicalRedisAddr(payload.Addr) != canonicalRedisAddr(activeAddr) || boundSSL != activeSSLMode {
		return "", errors.New("stored postgresPassword is bound to a different PostgreSQL endpoint or SSL mode; re-enter or reset postgresPassword")
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
