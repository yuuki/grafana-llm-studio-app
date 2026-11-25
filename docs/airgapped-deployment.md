# Air-Gapped / Offline Deployment & Operations Guide

This guide covers packaging, transferring, deploying, and operating `yuuki-llmstudio-app` in **strictly offline / air-gapped environments** with zero runtime outbound internet access.

> **Using an internet-connected environment?** You can install `grafana-llm-app` normally and connect to hosted OpenAI or OpenAI-compatible APIs — see [`docs/openai-compatible-apis.md`](openai-compatible-apis.md). The upstream Prometheus / VictoriaMetrics query guardrails in [Section 3](#3-upstream-prometheus--victoriametrics-query-limits-final-dos-boundary) apply to all environments.

## 1. Staging Preparation (Internet-Connected Staging Host)

### 1.1 Pin and Export Container Images
Record immutable image digests (`@sha256:...`) wherever possible before exporting with `docker save`.

```bash
# Pull pinned versions (Grafana OSS official image is grafana/grafana since 12.4.0):
# For Grafana 13.x (recommended, React 19): grafana/grafana:13.3.1
# For Grafana 12.4.x (React 18):            grafana/grafana:12.4.15
docker pull grafana/grafana:13.3.1
docker pull prom/prometheus:v3.5.0
# Pin the vLLM image tag/digest validated for your target model (e.g., vllm/vllm-openai:<pinned-tag>)

docker save \
  grafana/grafana:13.3.1 \
  prom/prometheus:v3.5.0 \
  -o airgap-images.tar
sha256sum airgap-images.tar > airgap-images.tar.sha256
```

### 1.2 Bundle Plugins and Offline Dependencies
1. **Custom App Plugin (`yuuki-llmstudio-app`):**
   ```bash
   # Optional: create offline caches if rebuilding inside the air-gapped network
   go mod vendor
   npm ci --prefer-offline

   # Build distributable plugin bundle (dist/)
   npm run verify:schemas
   npm run build
   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=vendor -o dist/gpx_yuuki_llmstudio_app_linux_amd64 ./pkg
   tar -czf yuuki-llmstudio-app-0.1.0.tar.gz -C dist .
   sha256sum yuuki-llmstudio-app-0.1.0.tar.gz > yuuki-llmstudio-app-0.1.0.tar.gz.sha256
   ```
2. **Official `grafana-llm-app` (Grafana 13.x: `v1.2.0` / Grafana 12.4.x: `v1.0.8`):**
   - Download the signed release archive for **`v1.2.0`** (`grafanaDependency: ">=13.0.0-0"`) on Grafana 13.3.1, or **`v1.0.8`** (`grafanaDependency: ">=9.5.2"`) on Grafana 12.4.15, verify its SHA-256 checksum, and transfer it offline.
   - **Never rely on runtime `GF_INSTALL_PLUGINS` downloads in air-gapped environments.**
   - **Grafana 13 upgrade notes:** Starting in Grafana 13.0, `grafana-cli` and `grafana-server` are removed in favor of `grafana cli` and `grafana server`. Dashboards and folders are also automatically migrated to Unified Storage on first startup—always back up the Grafana database before upgrading from 12.x to 13.x.

### 1.3 Pin Model Weights and Tokenizer Revisions
- Download model weights, `config.json`, `tokenizer.json`, `tokenizer_config.json`, and any chat template / custom encoding files pinned to an exact 40-character Hugging Face commit SHA (never a floating branch name such as `main`).
- Generate a SHA-256 checksum manifest across all `.safetensors` and configuration files, verify checksums inside the air-gapped environment, and mount the model directory read-only into vLLM.
- Always launch vLLM against the local filesystem path with `HF_HUB_OFFLINE=1` and `TRANSFORMERS_OFFLINE=1`.

## 2. Air-Gapped Deployment Configuration

### 2.1 Explicit Unsigned Plugin Allowlist & Offline Grafana Hardening
Allow **only** `yuuki-llmstudio-app` in `grafana.ini` or environment variables (never use wildcards):

```ini
[plugins]
allow_loading_unsigned_plugins = yuuki-llmstudio-app
public_key_retrieval_disabled = true
plugin_admin_enabled = false

[analytics]
reporting_enabled = false
check_for_updates = false
check_for_plugin_updates = false

[news]
news_feed_enabled = false

[security]
disable_gravatar = true

[snapshots]
external_enabled = false
```

Equivalent environment variables:
```bash
GF_PLUGINS_ALLOW_LOADING_UNSIGNED_PLUGINS=yuuki-llmstudio-app
GF_PLUGINS_PUBLIC_KEY_RETRIEVAL_DISABLED=true
GF_PLUGINS_PLUGIN_ADMIN_ENABLED=false
GF_ANALYTICS_REPORTING_ENABLED=false
GF_ANALYTICS_CHECK_FOR_UPDATES=false
GF_ANALYTICS_CHECK_FOR_PLUGIN_UPDATES=false
GF_NEWS_NEWS_FEED_ENABLED=false
GF_SECURITY_DISABLE_GRAVATAR=true
GF_SNAPSHOTS_EXTERNAL_ENABLED=false
```

> **Note:** `deploy/grafana/provisioning/plugins/llm-studio-dev.yaml` and the `sample-metrics` service are for local development/testing only. Do **not** deploy them to production air-gapped environments, and pin container images by `@sha256:...` digest.

### 2.2 Configuring `grafana-llm-app` for Local vLLM
- **Provider:** `Custom OpenAI-compatible` (`provider: "custom"`)
- **URL:** Internal vLLM endpoint (e.g., `https://vllm.internal:8000`, API path `/v1`)
- **Transport Security:** Even inside an air-gapped network, protect vLLM with TLS and `--api-key` (Bearer auth) or front-end mTLS, storing the API key in `grafana-llm-app`'s `secureJsonData`.
- **Model Mapping:**
  - `base` -> vLLM `--served-model-name` (e.g., `glm-5.3-flash-local`)
  - `large` -> higher-accuracy served model name (or the same name in single-model deployments)
- **Recommended vLLM Startup Flags:**
  - `HF_HUB_OFFLINE=1`, `TRANSFORMERS_OFFLINE=1`
  - Digest-pinned image (`vllm/vllm-openai@sha256:...`)
  - `--served-model-name <fixed-name>`
  - `--api-key <internal-secret>` (or reverse-proxy mTLS / Bearer enforcement)
  - `--max-num-seqs <env-limit>` (protects global inference server concurrency)
  - Explicit Structured Outputs backend (e.g., `--structured-outputs-config.backend auto`)
  - For reasoning models (GLM / DeepSeek / Kimi families), set `--reasoning-parser <model-parser>` so reasoning tokens are separated from `message.content`.
  - Where chat template defaults require tuning (such as `clear_thinking` on GLM-5.3), pin them in vLLM deployment flags (e.g., `--default-chat-template-kwargs`), because `grafana-llm-app` does not relay `extra_body`.

### 2.3 Dedicated LLM Service Account Permissions (Grafana OSS 12.4.15 / 13.3.1)
1. Create a Grafana service account dedicated to `yuuki-llmstudio-app` and assign the minimum OSS basic role **`Viewer`** (the lowest built-in role containing `plugins.app:access`; never grant `Editor` or `Admin`).
2. To prevent SSRF and token exfiltration, the Go backend restricts `grafanaUrl` in App Config to **loopback (`localhost` / `127.0.0.1` / `::1`) or the same origin as Grafana's `AppURL`**, and disables HTTP redirects.
3. Store the token only in `secureJsonData` (`llmServiceAccountToken`); never commit it to Git or plaintext `.env` files.
4. **Cluster-Wide LLM Rate Limiting:** In-plugin budgets (`maxConcurrentJobs`, `maxRepairAttempts`, `maxOutputTokens`) govern only requests originating from `yuuki-llmstudio-app`. If `grafana-llm-app` or vLLM is shared across multiple plugins or teams, enforce per-client rate limits, concurrency caps, and timeouts at the reverse proxy (Nginx / Envoy) or vLLM layer as well.

## 3. Upstream Prometheus / VictoriaMetrics Query Limits (Final DoS Boundary)

Browser-side dry-run caps (`now-5m`, `maxDataPoints=10`, concurrency `<= 2`, `10s` timeout) are **UX guardrails against accidental mistakes, not a DoS security boundary**. Always enforce hard query limits upstream.

### Example Prometheus Flags
```text
--query.timeout=15s
--query.max-concurrency=10
--query.max-samples=10000000
```

### Example VictoriaMetrics (`vmsingle` / `vmselect`) Flags
```text
-search.maxQueryDuration=15s
-search.maxConcurrentRequests=10
-search.maxSeries=30000
-search.maxSamplesPerQuery=10000000
-search.maxUniqueTimeseries=30000
```

### Reverse Proxy Settings (Fronting Grafana & vLLM)

- **Non-Streaming Baseline (`enableSseProgress: false`, default):** An LLM request waits up to `llmTimeoutSeconds` (default `45s`) and `jobDeadlineSeconds` (default `90s`). Configure reverse-proxy read timeouts to at least `jobDeadlineSeconds` (e.g., `120s`).
- **Optional SSE Stage-Progress Streaming (`enableSseProgress: true`):**
  - The backend emits decoupled application-level lifecycle stage events (`generating` -> `validating_ir` -> `awaiting_preview`, or `repairing` -> `validating_ir` -> `awaiting_preview`) plus `: keepalive` comment heartbeats every `15s` over Grafana `CallResource` (`Content-Type: text/event-stream; charset=utf-8`, `Cache-Control: no-cache, no-transform`, `X-Accel-Buffering: no`). Raw LLM tokens and unvalidated JSON fragments are **never** streamed or rendered.
  - Reverse proxies fronting Grafana **must** disable response buffering on the plugin resource endpoint, use HTTP/1.1 (or HTTP/2 end-to-end without buffering), and align idle timeouts above `jobDeadlineSeconds`:

#### Example Nginx Configuration (Scoped to Studio Resource Endpoints)

```nginx
location /api/plugins/yuuki-llmstudio-app/resources/ {
    proxy_pass http://grafana_upstream;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header Connection "";

    # Required for incremental SSE stage-progress flushing:
    proxy_buffering off;
    proxy_cache off;
    gzip off;

    # Align idle/read timeouts above jobDeadlineSeconds (default 90s; max 180s):
    proxy_read_timeout 180s;
    proxy_send_timeout 180s;
}
```

#### Example Envoy Route Configuration

```yaml
routes:
  - match:
      prefix: "/api/plugins/yuuki-llmstudio-app/resources/"
    route:
      cluster: grafana_upstream
      timeout: 180s
      idle_timeout: 180s
```

- **Graceful Fallback:** If an admin keeps `enableSseProgress: false`, the backend returns standard `application/json` even if a client sends `Accept: text/event-stream`. If `enableSseProgress: true` is enabled behind a proxy or browser that rejects SSE (`406`/`501`) or lacks streaming support, the Studio frontend automatically falls back to non-streaming JSON requests; if a proxy drops an SSE connection mid-flight after a `jobId` has been emitted, the frontend recovers the validated state via `GET /jobs/{id}` rather than issuing a duplicate LLM request.
