# ADR-001: Unified LLM Routing via `grafana-llm-app` & Structured Output Constraints

- **Status:** Accepted (2026-11-22)
- **Target Versions:** `Grafana OSS v12.4.15 / v13.3.1`, `grafana-llm-app v1.0.8 (12.4.x) / v1.2.0 (13.x)`, `llmclient v0.20.0`, `go-openai v1.41.1 / v1.41.2 / v1.42.0`, `vLLM >= v0.12.0`

## Context
Scattering local inference server (vLLM) URLs, provider credentials, and model mappings inside custom App plugins duplicates credential management and governance. In addition, nesting transport-level retries with semantic repair loops amplifies inference load during partial outages.

## Decision
1. All LLM traffic is unified along a single path: **`Frontend -> Custom App Go Backend -> Grafana plugin resource API -> grafana-llm-app -> local vLLM or internet HTTPS OpenAI-compatible API`**. No direct LLM connection or duplicate generic LLM proxy is added to this plugin.
2. The Go backend uses the official `github.com/grafana/grafana-llm-app/llmclient` (`v0.20.0`) rather than browser-side `@grafana/llm`, so prompt construction, deterministic IR validation, and job budgets remain centrally controlled in the backend.
3. Transport retries (max 1 retry for transient network failures or HTTP `502`/`503`/`504` / wrapped `520`/`522`/`524`) are strictly separated from semantic syntax repairs (max 2 attempts per job); HTTP `429` rate limits and `4xx` errors are never automatically retried.
4. To support both local vLLM and internet-hosted OpenAI / OpenAI-compatible providers (OpenAI, Azure OpenAI, OpenRouter, Groq, DeepSeek API, Together, Fireworks, Gemini OpenAI-compatible) through `grafana-llm-app`, App Config exposes `structuredOutputMode` (`json_schema` [default] or `json_object`), `tokenLimitField` (`max_completion_tokens` [default] or `max_tokens`), and optional `reasoningEffort` (default `""` omitted so non-reasoning OpenAI-compatible models do not fail with HTTP `400`).

## Verified Technical Constraints & Minimal Mitigations
Based on direct source inspection of `grafana-llm-app` (`packages/grafana-llm-app/pkg/plugin/llm_provider.go`, `openai_provider.go`) and contract tests (`pkg/llm/contract_test.go`), the following concrete constraints were identified and mitigated:

1. **Numeric `enum` Rejected with HTTP 400:**
   - **Reproduction:** Sending a JSON Schema containing a numeric `enum` (e.g., `"enum": [0, 1]`) inside `response_format.json_schema.schema` to `grafana-llm-app` fails `json.Unmarshal` with HTTP `400` because `go-openai/jsonschema.Definition.Enum` is typed as `[]string`.
   - **Mitigation:** Canonical schemas (`schemas/dashboard-ir.schema.json`, `schemas/dashboard-patch.schema.json`) use string-only `enum` values and express integer bounds via `minimum`/`maximum`. `scripts/verify-schema-sync.mjs` enforces this continuously.
2. **Dropped Schema Keywords During Relay, `strict: false`, and `json_object` Fallback:**
   - **Reproduction:** Keywords absent from `go-openai/jsonschema.Definition` (`minimum`, `maximum`, `minLength`, `maxLength`, `pattern`, `minItems`, `maxItems`) as well as non-standard OpenAI fields (`extra_body`, `chat_template_kwargs`) are dropped during relay by `grafana-llm-app`. Furthermore, OpenAI's `strict: true` mode requires all properties to be in `required` with nullable union types (`["string", "null"]`), which `go-openai/jsonschema.Definition.Type` (single string) cannot represent, causing schema rejection on optional fields (`description`, `legendFormat`, `thresholds`, etc.). Some third-party internet OpenAI-compatible APIs (e.g., DeepSeek API) also do not accept `response_format.type = "json_schema"` or `max_completion_tokens`.
   - **Mitigation:** In default `json_schema` mode, set `ResponseFormat.JSONSchema.Strict = false` so OpenAI/vLLM enforce object structure, `required`, `additionalProperties: false`, and string enums (including `datasourceRef: ["selected_prometheus"]`). In `json_object` mode, embed the canonical JSON Schema into the system message and send `response_format: {"type":"json_object"}`. In both modes, the Go backend's deterministic validator re-enforces all length/range/regex bounds, required numeric field presence, PromQL AST/regex safety, and catalog membership.
   - **Minimal Future Upstream Fix:** Forward `response_format.json_schema.schema` as `json.RawMessage` in `grafana-llm-app`'s proxy layer (do **not** bypass `grafana-llm-app` directly to LLM endpoints because of this constraint).
3. **`grafana-llm-app` HTTP 500 Wrapping of Upstream Errors & `go-openai` Error Unwrap Order:**
   - **Reproduction:** When upstream vLLM or an internet OpenAI-compatible endpoint returns HTTP `502`/`503`/`504` (or edge `520`/`522`/`524`), `grafana-llm-app`'s `openai_provider.go` may wrap the error as HTTP `500` `{"error":"<upstream status/message>"}`. `go-openai v1.41.1` decodes non-standard error JSON into `*openai.RequestError` whose wrapped `Err` contains an empty `*openai.APIError{HTTPStatusCode: 0}`.
   - **Mitigation:** In `pkg/llm/grafana_llm.go` (`isRetryableTransportError`), inspect `*openai.RequestError` **before** `*openai.APIError`, retrying at most once for HTTP `502`/`503`/`504` or HTTP `500` bodies clearly wrapping upstream `502`/`503`/`504`/`520`/`522`/`524`/connection-reset/unexpected-EOF transient errors (never retrying `400`/`401`/`403`/`429` or context cancellation). Verified continuously by `pkg/llm/contract_test.go`.
4. **Development Mock Mode Isolation:**
   - `llmMode=mock` requires explicit `allowMockInDev=true` and is strictly for development/testing. Production `llmMode=grafana-llm-app` never silently falls back to mock mode on failure.
