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
