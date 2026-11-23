package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/grafana/grafana-llm-app/llmclient"
	"github.com/yuuki/grafana-llm-studio-app/pkg/bench"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("llm-bench", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		grafanaURL       = fs.String("grafana-url", envOrDefault("GRAFANA_URL", "http://127.0.0.1:3000"), "Grafana base URL hosting grafana-llm-app")
		saToken          = fs.String("sa-token", os.Getenv("GRAFANA_SA_TOKEN"), "Grafana Service Account token (Viewer role) for grafana-llm-app (or env GRAFANA_SA_TOKEN)")
		preset           = fs.String("preset", "", "Provider preset (use --list-presets to inspect available presets)")
		providerLabel    = fs.String("provider-label", "", "Human-readable provider/model label for the report (e.g. openai/gpt-4.1-mini)")
		modelTier        = fs.String("model-tier", "", "grafana-llm-app logical model tier: base or large")
		structuredOutput = fs.String("structured-output", "", "Structured output mode: json_schema or json_object")
		tokenLimitField  = fs.String("token-limit-field", "", "Token limit field: max_completion_tokens or max_tokens")
		reasoningEffort  = fs.String("reasoning-effort", "", "Optional reasoning_effort: \"\", low, medium, high")
		maxOutputTokens  = fs.Int("max-output-tokens", 0, "Maximum output tokens per completion (overrides preset when > 0)")
		timeout          = fs.Duration("timeout", 0, "Per-request LLM timeout (overrides preset when > 0)")
		iterations       = fs.Int("iterations", 1, "Number of iterations per scenario")
		scenariosFlag    = fs.String("scenarios", "", "Comma-separated scenario IDs or kinds (generate,patch,repair) to run")
		prometheusURL    = fs.String("prometheus-url", envOrDefault("PROMETHEUS_URL", ""), "Optional live Prometheus base URL (e.g. http://127.0.0.1:9090) for query execution validation")
		jsonOut          = fs.String("json-out", "", "Optional path to write full JSON benchmark report")
		mdOut            = fs.String("md-out", "", "Optional path to write Markdown benchmark report")
		failOnError      = fs.Bool("fail-on-error", true, "Exit with non-zero status if any benchmark scenario fails")
		listPresets      = fs.Bool("list-presets", false, "Print built-in provider presets and exit")
		listScenarios    = fs.Bool("list-scenarios", false, "Print built-in benchmark scenarios and exit")
	)

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *listPresets {
		fmt.Println("Available provider presets:")
		for _, p := range bench.Presets() {
			fmt.Printf("  %-18s tier=%-5s format=%-11s limit=%-21s reasoning=%-4q maxTokens=%-4d %s\n",
				p.Name, p.ModelTier, p.StructuredOutputMode, p.TokenLimitField, p.ReasoningEffort, p.MaxOutputTokens, p.Description)
		}
		return 0
	}

	if *listScenarios {
		fmt.Println("Available benchmark scenarios:")
		for _, sc := range bench.DefaultScenarios() {
			fmt.Printf("  %-26s kind=%-8s %s\n", sc.ID, sc.Kind, sc.Description)
		}
		return 0
	}

	// Track which flags were explicitly passed so they override preset defaults cleanly.
	explicit := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		explicit[f.Name] = true
	})

	cfg := bench.Config{
		ProviderLabel: strings.TrimSpace(*providerLabel),
		LLMConfig: llm.GrafanaLLMConfig{
			GrafanaURL:           strings.TrimSpace(*grafanaURL),
			ServiceAccountToken:  strings.TrimSpace(*saToken),
			ModelTier:            llmclient.ModelBase,
			StructuredOutputMode: llm.StructuredOutputJSONSchema,
			TokenLimitField:      llm.TokenLimitMaxCompletionTokens,
			MaxOutputTokens:      2048,
			RequestTimeout:       45 * time.Second,
			MaxTransportRetries:  1,
		},
		Iterations:    *iterations,
		PrometheusURL: strings.TrimSpace(*prometheusURL),
	}

	if strings.TrimSpace(*preset) != "" {
		if err := bench.ApplyPreset(*preset, &cfg); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 2
		}
	}

	if explicit["provider-label"] {
		cfg.ProviderLabel = strings.TrimSpace(*providerLabel)
	}
	if explicit["model-tier"] {
		cfg.LLMConfig.ModelTier = llmclient.Model(strings.TrimSpace(*modelTier))
	}
	if explicit["structured-output"] {
		cfg.LLMConfig.StructuredOutputMode = strings.TrimSpace(*structuredOutput)
	}
	if explicit["token-limit-field"] {
		cfg.LLMConfig.TokenLimitField = strings.TrimSpace(*tokenLimitField)
	}
	if explicit["reasoning-effort"] {
		cfg.LLMConfig.ReasoningEffort = strings.TrimSpace(*reasoningEffort)
	}
	if explicit["max-output-tokens"] && *maxOutputTokens > 0 {
		cfg.LLMConfig.MaxOutputTokens = *maxOutputTokens
	}
	if explicit["timeout"] && *timeout > 0 {
		cfg.LLMConfig.RequestTimeout = *timeout
	}
	if strings.TrimSpace(*scenariosFlag) != "" {
		cfg.ScenarioFilter = splitCSV(*scenariosFlag)
	}

	if cfg.LLMConfig.ServiceAccountToken == "" {
		fmt.Fprintln(os.Stderr, "error: --sa-token or GRAFANA_SA_TOKEN is required (Viewer-role Grafana Service Account token)")
		return 2
	}

	runner, err := bench.NewRunner(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: initialize benchmark runner: %v\n", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	report, runErr := runner.Run(ctx)
	md := bench.RenderMarkdown(report)
	fmt.Print(md)

	if strings.TrimSpace(*jsonOut) != "" {
		if err := writeReportFile(*jsonOut, func(f *os.File) error {
			return bench.WriteJSON(f, report)
		}); err != nil {
			fmt.Fprintf(os.Stderr, "error: write JSON report: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "Wrote JSON report to %s\n", *jsonOut)
	}
	if strings.TrimSpace(*mdOut) != "" {
		if err := writeReportFile(*mdOut, func(f *os.File) error {
			_, err := f.WriteString(md)
			return err
		}); err != nil {
			fmt.Fprintf(os.Stderr, "error: write Markdown report: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "Wrote Markdown report to %s\n", *mdOut)
	}

	if runErr != nil {
		fmt.Fprintf(os.Stderr, "benchmark aborted: %v\n", runErr)
		return 1
	}
	if *failOnError && report.Summary.PassedRuns < report.Summary.TotalRuns {
		return 1
	}
	return 0
}

func writeReportFile(path string, fn func(*os.File) error) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func splitCSV(s string) []string {
	raw := strings.Split(s, ",")
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if t := strings.TrimSpace(item); t != "" {
			out = append(out, t)
		}
	}
	return out
}
