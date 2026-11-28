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

type benchCLIFlags struct {
	grafanaURL       string
	saToken          string
	preset           string
	providerLabel    string
	modelTier        string
	structuredOutput string
	tokenLimitField  string
	reasoningEffort  string
	maxOutputTokens  int
	timeout          time.Duration
	iterations       int
	scenariosFlag    string
	prometheusURL    string
	jsonOut          string
	mdOut            string
	failOnError      bool
	listPresets      bool
	listScenarios    bool
}

func run(args []string) int {
	flags, explicit, ok := parseCLIFlags(args)
	if !ok {
		return 2
	}
	if flags.listPresets {
		printPresets()
		return 0
	}
	if flags.listScenarios {
		printScenarios()
		return 0
	}

	cfg, err := buildBenchConfig(flags, explicit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
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

	if err := writeOptionalReports(flags.jsonOut, flags.mdOut, md, report); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	if runErr != nil {
		fmt.Fprintf(os.Stderr, "benchmark aborted: %v\n", runErr)
		return 1
	}
	if flags.failOnError && report.Summary.PassedRuns < report.Summary.TotalRuns {
		return 1
	}
	return 0
}

func parseCLIFlags(args []string) (benchCLIFlags, map[string]bool, bool) {
	fs := flag.NewFlagSet("llm-bench", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var f benchCLIFlags
	fs.StringVar(&f.grafanaURL, "grafana-url", envOrDefault("GRAFANA_URL", "http://127.0.0.1:3000"), "Grafana base URL hosting grafana-llm-app")
	fs.StringVar(&f.saToken, "sa-token", os.Getenv("GRAFANA_SA_TOKEN"), "Grafana Service Account token (Viewer role) for grafana-llm-app (or env GRAFANA_SA_TOKEN)")
	fs.StringVar(&f.preset, "preset", "", "Provider preset (use --list-presets to inspect available presets)")
	fs.StringVar(&f.providerLabel, "provider-label", "", "Human-readable provider/model label for the report (e.g. openai/gpt-4.1-mini)")
	fs.StringVar(&f.modelTier, "model-tier", "", "grafana-llm-app logical model tier: base or large")
	fs.StringVar(&f.structuredOutput, "structured-output", "", "Structured output mode: json_schema or json_object")
	fs.StringVar(&f.tokenLimitField, "token-limit-field", "", "Token limit field: max_completion_tokens or max_tokens")
	fs.StringVar(&f.reasoningEffort, "reasoning-effort", "", "Optional reasoning_effort: \"\", low, medium, high")
	fs.IntVar(&f.maxOutputTokens, "max-output-tokens", 0, "Maximum output tokens per completion (overrides preset when > 0)")
	fs.DurationVar(&f.timeout, "timeout", 0, "Per-request LLM timeout (overrides preset when > 0)")
	fs.IntVar(&f.iterations, "iterations", 1, "Number of iterations per scenario")
	fs.StringVar(&f.scenariosFlag, "scenarios", "", "Comma-separated scenario IDs or kinds (generate,patch,repair) to run")
	fs.StringVar(&f.prometheusURL, "prometheus-url", envOrDefault("PROMETHEUS_URL", ""), "Optional live Prometheus base URL (e.g. http://127.0.0.1:9090) for query execution validation")
	fs.StringVar(&f.jsonOut, "json-out", "", "Optional path to write full JSON benchmark report")
	fs.StringVar(&f.mdOut, "md-out", "", "Optional path to write Markdown benchmark report")
	fs.BoolVar(&f.failOnError, "fail-on-error", true, "Exit with non-zero status if any benchmark scenario fails")
	fs.BoolVar(&f.listPresets, "list-presets", false, "Print built-in provider presets and exit")
	fs.BoolVar(&f.listScenarios, "list-scenarios", false, "Print built-in benchmark scenarios and exit")

	if err := fs.Parse(args); err != nil {
		return benchCLIFlags{}, nil, false
	}

	// Track which flags were explicitly passed so they override preset defaults cleanly.
	explicit := make(map[string]bool)
	fs.Visit(func(fl *flag.Flag) {
		explicit[fl.Name] = true
	})
	return f, explicit, true
}

func printPresets() {
	fmt.Println("Available provider presets:")
	for _, p := range bench.Presets() {
		fmt.Printf("  %-18s tier=%-5s format=%-11s limit=%-21s reasoning=%-4q maxTokens=%-4d %s\n",
			p.Name, p.ModelTier, p.StructuredOutputMode, p.TokenLimitField, p.ReasoningEffort, p.MaxOutputTokens, p.Description)
	}
}

func printScenarios() {
	fmt.Println("Available benchmark scenarios:")
	for _, sc := range bench.DefaultScenarios() {
		fmt.Printf("  %-26s kind=%-8s %s\n", sc.ID, sc.Kind, sc.Description)
	}
}

func buildBenchConfig(f benchCLIFlags, explicit map[string]bool) (bench.Config, error) {
	cfg := bench.Config{
		ProviderLabel: strings.TrimSpace(f.providerLabel),
		LLMConfig: llm.GrafanaLLMConfig{
			GrafanaURL:           strings.TrimSpace(f.grafanaURL),
			ServiceAccountToken:  strings.TrimSpace(f.saToken),
			ModelTier:            llmclient.ModelBase,
			StructuredOutputMode: llm.StructuredOutputJSONSchema,
			TokenLimitField:      llm.TokenLimitMaxCompletionTokens,
			MaxOutputTokens:      2048,
			RequestTimeout:       45 * time.Second,
			MaxTransportRetries:  1,
		},
		Iterations:    f.iterations,
		PrometheusURL: strings.TrimSpace(f.prometheusURL),
	}

	if strings.TrimSpace(f.preset) != "" {
		if err := bench.ApplyPreset(f.preset, &cfg); err != nil {
			return bench.Config{}, err
		}
	}

	if explicit["provider-label"] {
		cfg.ProviderLabel = strings.TrimSpace(f.providerLabel)
	}
	if explicit["model-tier"] {
		cfg.LLMConfig.ModelTier = llmclient.Model(strings.TrimSpace(f.modelTier))
	}
	if explicit["structured-output"] {
		cfg.LLMConfig.StructuredOutputMode = strings.TrimSpace(f.structuredOutput)
	}
	if explicit["token-limit-field"] {
		cfg.LLMConfig.TokenLimitField = strings.TrimSpace(f.tokenLimitField)
	}
	if explicit["reasoning-effort"] {
		cfg.LLMConfig.ReasoningEffort = strings.TrimSpace(f.reasoningEffort)
	}
	if explicit["max-output-tokens"] && f.maxOutputTokens > 0 {
		cfg.LLMConfig.MaxOutputTokens = f.maxOutputTokens
	}
	if explicit["timeout"] && f.timeout > 0 {
		cfg.LLMConfig.RequestTimeout = f.timeout
	}
	if strings.TrimSpace(f.scenariosFlag) != "" {
		cfg.ScenarioFilter = splitCSV(f.scenariosFlag)
	}

	if cfg.LLMConfig.ServiceAccountToken == "" {
		return bench.Config{}, fmt.Errorf("--sa-token or GRAFANA_SA_TOKEN is required (Viewer-role Grafana Service Account token)")
	}
	return cfg, nil
}

func writeOptionalReports(jsonOut, mdOut, md string, report bench.Report) error {
	if strings.TrimSpace(jsonOut) != "" {
		if err := writeReportFile(jsonOut, func(f *os.File) error {
			return bench.WriteJSON(f, report)
		}); err != nil {
			return fmt.Errorf("write JSON report: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Wrote JSON report to %s\n", jsonOut)
	}
	if strings.TrimSpace(mdOut) != "" {
		if err := writeReportFile(mdOut, func(f *os.File) error {
			_, err := f.WriteString(md)
			return err
		}); err != nil {
			return fmt.Errorf("write Markdown report: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Wrote Markdown report to %s\n", mdOut)
	}
	return nil
}

func writeReportFile(path string, fn func(*os.File) error) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
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
