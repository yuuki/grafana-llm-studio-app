# Changelog

All notable changes to **LLM Dashboard Studio (`yuuki-llmstudio-app`)** will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Multi-architecture Go backend cross-compilation (`linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`) with embedded `grafana-plugin-sdk-go` build metadata.
- Automated GitHub Actions release workflow (`.github/workflows/release.yml`) triggered on `v*` version tags and manual `workflow_dispatch` runs, packaging `yuuki-llmstudio-app-<version>.zip`, `yuuki-llmstudio-app-<version>.tar.gz`, `yuuki-llmstudio-app-<version>.zip.sha1`, `SHA256SUMS`, and an optional offline air-gapped bundle (`yuuki-llmstudio-app-<version>-airgap.tar.gz`).
- Grafana plugin signing integration (`@grafana/sign-plugin@3.3.3`) supporting Community and Private signing workflows, post-signing `MANIFEST.txt` integrity verification, and documentation for Grafana plugin catalog submission (`docs/plugin-signing-and-publishing.md`).
- Optional decoupled SSE stage-progress streaming over `CallResource` (`enableSseProgress`).
- Optional Redis/Valkey HA job store backend (`jobStoreBackend: "redis"`).
- Optional PostgreSQL HA job store backend (`jobStoreBackend: "postgres"`) with transactional concurrency locking, `LISTEN`/`NOTIFY` cross-replica cancellation, destination-bound `postgresPassword` credentials, and optional `LLMSTUDIO_POSTGRES_ADDR` operator pinning.
- First-class VictoriaMetrics MetricsQL datasource adapter.
- Expanded Dashboard IR v1.1 (7 panel types, multi-query panels, template variables, safe existing dashboard import).
- English (default) and Japanese (`ja`) UI internationalization via Grafana's shared `i18next` runtime.

### Changed
- Development, CI and releases use Node.js 26 and Go 1.27.4 (`mise.toml`).
- The PromQL validator uses the Prometheus parser `v0.311.3` through `parser.NewParser`, with every experimental parser option off as before.

### Security
- Update the Go modules with vulnerabilities reachable from the backend: `google.golang.org/grpc`, OpenTelemetry, `golang.org/x/net`, `golang.org/x/text`, `github.com/klauspost/compress`, `github.com/getkin/kin-openapi` and `github.com/prometheus/prometheus`.

## [0.1.0] - 2026-11-22

### Added
- Initial MVP release of `yuuki-llmstudio-app` supporting Grafana OSS `13.3.1` (React 19) and `12.4.15` (React 18).
- Metric-grounded dashboard generation, partial panel patching, undo history, and syntax-only repair routed through `grafana-llm-app`.
- Dual-adapter preview-to-save parity (`@grafana/scenes` live preview + `@grafana/grafana-foundation-sdk` Classic Dashboard JSON export) with create-only dashboard saving.
- Strict Go backend IR validation, PromQL AST/regex safety checks, per-user job isolation, and bounded dry-run validation.
