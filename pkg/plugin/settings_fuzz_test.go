package plugin_test

import (
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/yuuki/grafana-llm-studio-app/pkg/plugin"
)

// FuzzLoadSettings feeds arbitrary jsonData, as an administrator or a
// provisioning file may write it, to LoadSettings. Accepted settings must stay
// inside the documented bounds, and grafanaUrl, which receives the service
// account token, must point to loopback or the trusted Grafana origin.
func FuzzLoadSettings(f *testing.F) {
	const trusted = "https://grafana.example.com"
	for _, s := range []string{
		`{}`,
		`{"llmMode":"mock","allowMockInDev":true,"allowedDatasourceUids":["prom-1"],"mockDelayMs":20000}`,
		`{"llmMode":"mock"}`,
		`{"grafanaUrl":"https://grafana.example.com/sub","maxPanels":99,"maxRepairAttempts":-1}`,
		`{"grafanaUrl":"http://[::1]:3000","maxOutputTokens":9000,"llmTimeoutSeconds":0}`,
		`{"grafanaUrl":"http://evil.example.com","modelTier":"large"}`,
		`{"grafanaUrl":"http://user:pass@localhost:3000"}`,
		`{"structuredOutputMode":"JSON_OBJECT","tokenLimitField":"max_tokens","reasoningEffort":" Low "}`,
		`{"datasourceDialects":{"prom-1":"metricsql"," ":"promql"},"allowedDatasourceUids":[" prom-1 ",""]}`,
		`{"jobStoreBackend":"redis","redisAddr":"redis:6379","redisDb":3,"redisKeyPrefix":"llmstudio:jobs"}`,
		`{"jobStoreBackend":"redis","redisAddr":"redis://x","redisDb":16}`,
	} {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		s, err := plugin.LoadSettings(backend.AppInstanceSettings{JSONData: raw}, trusted)
		if err != nil {
			return
		}

		u, err := url.Parse(strings.TrimSpace(s.GrafanaURL))
		if err != nil {
			t.Fatalf("accepted grafanaUrl %q does not parse: %v", s.GrafanaURL, err)
		}
		if u.User != nil {
			t.Fatalf("accepted grafanaUrl %q embeds credentials", s.GrafanaURL)
		}
		host := strings.ToLower(u.Hostname())
		ip := net.ParseIP(host)
		loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
		if !loopback && !strings.EqualFold(u.Scheme+"://"+u.Host, trusted) {
			t.Fatalf("accepted grafanaUrl %q is neither loopback nor %s", s.GrafanaURL, trusted)
		}

		if s.LLMMode == plugin.LLMModeMock && !s.AllowMockInDev {
			t.Fatal("mock mode accepted without allowMockInDev")
		}
		if s.LLMMode != plugin.LLMModeMock && s.MockDelayMs != 0 {
			t.Fatalf("mockDelayMs = %d outside mock mode", s.MockDelayMs)
		}
		for _, b := range []struct {
			name          string
			got, min, max int
		}{
			{"maxPanels", s.MaxPanels, 1, 6},
			{"maxRepairAttempts", s.EffectiveMaxRepairs, 0, 2},
			{"maxOutputTokens", s.MaxOutputTokens, 1, 8192},
			{"llmTimeoutSeconds", s.LLMTimeoutSeconds, 1, 120},
			{"jobDeadlineSeconds", s.JobDeadlineSeconds, 1, 180},
			{"maxConcurrentJobs", s.MaxConcurrentJobs, 1, 16},
			{"validationTimeoutSeconds", s.ValidationTimeoutSec, 1, 30},
			{"maxConcurrentQueries", s.MaxConcurrentQueries, 1, 4},
			{"jobTtlMinutes", s.JobTTLMinutes, 1, 1440},
			{"mockDelayMs", s.MockDelayMs, 0, 10000},
		} {
			if b.got < b.min || b.got > b.max {
				t.Fatalf("%s = %d, want %d..%d", b.name, b.got, b.min, b.max)
			}
		}
		if s.JobStoreBackend == plugin.JobStoreBackendRedis && (s.RedisDB < 0 || s.RedisDB > 15) {
			t.Fatalf("redisDb = %d, want 0..15", s.RedisDB)
		}
		for _, uid := range s.AllowedDatasourceUIDs {
			if uid == "" || uid != strings.TrimSpace(uid) {
				t.Fatalf("allowedDatasourceUids keeps %q", uid)
			}
		}
	})
}
