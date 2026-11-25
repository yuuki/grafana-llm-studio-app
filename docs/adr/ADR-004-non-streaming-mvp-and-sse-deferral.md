# ADR-004: Non-Streaming Structured Output in MVP & SSE Deferral

- **Status:** Accepted (2026-11-22)
- **Target Versions:** `Grafana OSS v12.4.15 / v13.3.1`, `grafana-plugin-sdk-go v0.278.0`

## Context
Token streaming or Server-Sent Events (SSE) progress updates can improve perceived responsiveness during LLM generation. However, `grafana-llm-app`'s browser streaming channel (Grafana Live WebSocket), Go `llmclient` streaming, and SSE over custom App Plugin `CallResource` are distinct transport mechanisms. Furthermore, incrementally applying partial, unvalidated JSON fragments into `@grafana/scenes` causes rendering errors and excessive datasource queries.

## Decision
1. The MVP adopts **non-streaming Structured Output generation** paired with explicit lifecycle states (`generating`, `validating_ir`, `awaiting_preview`, `repairing`, `cancelled`, `failed`) and end-to-end **cancellation** (frontend RxJS `unsubscribe` / `AbortController` + backend `POST /jobs/{id}/cancel` + `context.CancelFunc`).
2. Drafts are presented only after LLM completion and deterministic Go backend validation succeed, and dry-run queries plus `@grafana/scenes` rendering run only when the user explicitly clicks **Validate & Preview**.
3. If SSE stage notifications are introduced in a future phase, live-environment verification is required beforehand for:
   - `http.Flusher`, chunk flushing, and client-disconnect propagation through `CallResource` on the target Grafana + `grafana-plugin-sdk-go` versions.
   - Reverse proxy configuration (`proxy_buffering off`, HTTP/1.1, and idle timeout alignment on Nginx / Envoy).
   - Clear separation between raw LLM token streaming and application-level stage progress notifications.

## Update: Optional Decoupled SSE Stage-Progress Streaming (2026-11-25)

All three prerequisites above were verified live against **Grafana OSS `13.3.1`** and **Grafana OSS `12.4.15`** using `grafana-plugin-sdk-go v0.278.0` (`scripts/verify-sse-callresource.mjs`, also executed in CI on both versions):

- **Incremental `http.Flusher` Chunk Delivery:** `httpadapter`'s `callResourceResponseWriter` implements `http.Flusher` and streams each flushed frame as a separate gRPC `CallResourceResponse` message. Both Grafana `13.3.1` and `12.4.15` flush `Content-Type: text/event-stream; charset=utf-8` and `X-Accel-Buffering: no` incrementally (`generating` arrives in ~30–40ms while a 1200ms mock LLM call is still sleeping; `validating_ir`, `awaiting_preview`, and `result` arrive ~1200ms later). Note that Grafana's HTTP middleware overwrites `Cache-Control` to `no-store`, so reverse proxies must still explicitly set `proxy_buffering off` and `gzip off`.
- **Client-Disconnect Cancellation Propagation:** Aborting the HTTP SSE request propagates through Grafana -> gRPC `CallResource` -> `r.Context().Done()` -> `opCtx`, cancelling the in-flight LLM call, transitioning the job to `cancelled`, and releasing the concurrency slot within ~20–50ms on both `12.4.15` and `13.3.1`.
- **Strict Decoupling & Opt-In Gate:**
  - Controlled by admin setting `enableSseProgress` (`jsonData`, default `false` — strictly opt-in). When `false`, the backend ignores `Accept: text/event-stream` and responds with standard `application/json`.
  - When `enableSseProgress: true` and the client sends `Accept: text/event-stream, application/json` on `POST /jobs/generate`, `POST /jobs/{id}/patch`, or `POST /jobs/{id}/repair`, the backend still executes **non-streaming structured output** LLM calls and emits ONLY small application-level lifecycle events (`event: stage` for `generating`/`repairing` -> `validating_ir` -> terminal status, plus `: keepalive` heartbeats every `15s`) followed by a single validated `event: result` (or `event: error`). Pre-flight errors (`400`/`401`/`403`/`404`/`409`/`422`/`429`/`503`) still return standard HTTP status codes and JSON because SSE headers are deferred until the first stage event.
  - Raw LLM tokens and unvalidated JSON fragments are **never** streamed or rendered into `@grafana/scenes`.
  - The frontend falls back gracefully to non-streaming requests if SSE is unavailable or rejected (`406`/`501`), and recovers via `GET /jobs/{id}` without re-issuing a duplicate LLM call if an intermediary proxy drops an SSE connection mid-flight after a `jobId` has been emitted.
  - Reverse-proxy configuration examples (Nginx `proxy_buffering off`, `proxy_http_version 1.1`, `gzip off`, `proxy_read_timeout`; Envoy `idle_timeout`) are documented in [`docs/airgapped-deployment.md`](../airgapped-deployment.md#reverse-proxy-settings-fronting-grafana--vllm).
