# Connecting OpenAI & OpenAI-Compatible APIs (via `grafana-llm-app`)

LLM Dashboard Studio works seamlessly in **internet-connected self-hosted Grafana environments** (OpenAI, Azure OpenAI, OpenRouter, Groq, DeepSeek API, Together AI, Fireworks AI, Google Gemini OpenAI-compatible, etc.) as well as **private / air-gapped networks** running local vLLM — using the same unified routing architecture.

All LLM calls **always** pass through Grafana's official [`grafana-llm-app`](https://github.com/grafana/grafana-llm-app):

```text
Frontend -> yuuki-llmstudio-app Go Backend -> Grafana Plugin Resource API -> grafana-llm-app -> HTTPS OpenAI-Compatible API
```

`yuuki-llmstudio-app` never opens direct outbound connections to external LLM providers and never stores provider API keys; `grafana-llm-app` remains the single control point for provider URLs, encrypted API keys (`secureJsonData.openAIKey`), and model tier mappings (`base` / `large`).

## 1. Data Privacy & Outbound Payload Boundary

When routing `grafana-llm-app` to an external internet-hosted API:
- **What IS sent to the LLM provider:** User natural-language prompt text (bounded to 1,000 runes), the selected scope filter (`labelName=labelValue`), narrowed Prometheus metric names / `HELP` / `TYPE` / label names (strictly bounded), the current draft IR (on partial edits), and sanitized PromQL syntax errors (on explicit repair).
- **What is NEVER sent to the LLM provider:** Raw time-series data points, Prometheus/Grafana credentials, service account tokens, session cookies, or arbitrary non-scoped metrics.

Ensure your organization's security policy permits sending metric names, `HELP` strings, and user prompts to the chosen external provider.

## 2. Recommended Provider Settings

Configure the **endpoint, API key, and `base`/`large` model mapping** in **`grafana-llm-app`**, and tune the **protocol options** in **`yuuki-llmstudio-app` App Config**.

| Provider | `grafana-llm-app` `provider` | `grafana-llm-app` `url` | `grafana-llm-app` `apiPath` | Studio `structuredOutputMode` | Studio `tokenLimitField` | Studio `reasoningEffort` |
|---|---|---|---|---|---|---|
| **OpenAI (GPT-4o / GPT-4.1)** | `openai` | `https://api.openai.com` | `/v1` | `json_schema` | `max_completion_tokens` | `""` *(Auto/Omit)* |
| **OpenAI (o3 / GPT-5 reasoning)** | `openai` | `https://api.openai.com` | `/v1` | `json_schema` | `max_completion_tokens` | `low` *(recommended)* |
| **Azure OpenAI** | `azure` | `https://<resource>.openai.azure.com` | *(Azure mapping)* | `json_schema` | `max_completion_tokens` | `""` or `low` *(for o-series)* |
| **OpenRouter** | `custom` | `https://openrouter.ai/api` | `/v1` | `json_schema` *(or `json_object` for unsupported models)* | `max_completion_tokens` | `""` *(or `low` for reasoning models)* |
| **Groq** | `custom` | `https://api.groq.com/openai` | `/v1` | `json_schema` *(or `json_object` depending on model)* | `max_completion_tokens` | `""` |
| **DeepSeek API** | `custom` | `https://api.deepseek.com` | `/v1` | **`json_object`** | **`max_tokens`** | `""` |
| **Together AI** | `custom` | `https://api.together.xyz` | `/v1` | `json_schema` *(or `json_object`)* | `max_tokens` *(or `max_completion_tokens`)* | `""` |
| **Fireworks AI** | `custom` | `https://api.fireworks.ai/inference` | `/v1` | `json_schema` | `max_completion_tokens` | `""` |
| **Google Gemini (OpenAI-compat)** | `custom` | `https://generativelanguage.googleapis.com/v1beta/openai` | `""` *(empty string)* | `json_schema` | `max_completion_tokens` | `""` *(or `low` for thinking models)* |
| **Local vLLM (Air-Gapped)** | `custom` | `https://vllm.internal:8000` | `/v1` | `json_schema` | `max_completion_tokens` | `""` *(or `low` for GLM-5.3)* |

> **Why these three compatibility options matter:**
> 1. **`reasoningEffort` defaults to `""` (omitted):** Standard non-reasoning models (`gpt-4o`, `gpt-4.1`, Llama, `deepseek-chat`, etc.) return HTTP `400` if `reasoning_effort` is sent. Select `low` only when targeting reasoning models (such as `o3`, `gpt-5`, or `GLM-5.3`). When `reasoningEffort` is set, `temperature` is automatically omitted to avoid reasoning-model conflicts.
> 2. **`structuredOutputMode: "json_object"`:** Providers that do not support OpenAI's `response_format.type = "json_schema"` (such as DeepSeek API) accept `response_format.type = "json_object"`. In this mode, the Go backend automatically embeds the canonical JSON Schema in the system prompt and still enforces full strict decoding, PromQL AST/regex validation, and catalog membership deterministically.
> 3. **`tokenLimitField: "max_tokens"`:** Modern OpenAI models (`o3`, `gpt-5`) require `max_completion_tokens`, whereas some third-party OpenAI-compatible APIs expect legacy `max_tokens`.

## 3. Setup Walkthrough

### Step 1: Configure `grafana-llm-app`
1. Install `grafana-llm-app` (`v1.2.0` on Grafana 13.x; `v1.0.8` on Grafana 12.4.x).
2. In **Administration -> Plugins -> LLM (`grafana-llm-app`)**:
   - Select **OpenAI**, **Azure OpenAI**, or **Custom OpenAI-compatible** (`provider: "custom"`).
   - Enter the provider URL, API path, and API key (stored encrypted in `secureJsonData`).
   - Map `base` (e.g., `gpt-4.1-mini`) and `large` (e.g., `gpt-4.1`) to your desired model IDs.
   - Click **Save & Test** and verify the health check passes.

### Step 2: Configure `yuuki-llmstudio-app`
1. Create a Grafana Service Account with the minimum OSS role **`Viewer`** and generate a token (`glsa_...`).
2. In **Administration -> Plugins -> LLM Dashboard Studio (`yuuki-llmstudio-app`)**:
   - Set **Allowed Prometheus Datasource UIDs** (e.g., `prom-main`).
   - Set **LLM Mode** to `grafana-llm-app`.
   - Keep **Grafana Internal URL** as `http://localhost:3000` (or the Grafana instance's own `AppURL` origin).
   - Select **Model Tier** (`base` or `large`), **Structured Output Mode**, **Token Limit Parameter**, and **Reasoning Effort** according to the table above.
   - Paste the `Viewer` service account token into `llmServiceAccountToken` (`secureJsonData`).
   - Save settings and verify the health check passes.

### Step 3: Optional Local Docker Compose Run with an Internet API

For quick internet-connected testing with Docker Compose (do **not** use in air-gapped environments):

```bash
make build
cp deploy/.env.example deploy/.env
cp deploy/grafana/provisioning/plugins/grafana-llm-app-openai.yaml.example \
   deploy/grafana/provisioning/plugins/grafana-llm-app-openai.yaml

export OPENAI_API_KEY="sk-..."
export OPENAI_BASE_URL="https://api.openai.com"
export OPENAI_API_PATH="/v1"
export OPENAI_MODEL_BASE="gpt-4.1-mini"
export OPENAI_MODEL_LARGE="gpt-4.1"

docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.openai.yml up -d
```

Then open `http://localhost:3000`, switch `yuuki-llmstudio-app` from `mock` to `grafana-llm-app` in **App Config**, and attach a `Viewer` service account token.

## 4. Rate Limits & Retries on Internet APIs

- **HTTP `429` (Rate Limit) and `4xx` errors are never automatically retried** by the Go backend to prevent quota burn or retry storms.
- Only transient network/gateway errors (`502`, `503`, `504`, plus wrapped `520`/`522`/`524` internet edge errors) are retried at most **once** at the transport layer, strictly separated from semantic syntax repair attempts.
- Tune `maxOutputTokens`, `llmTimeoutSeconds`, `maxConcurrentJobs`, and `maxRepairAttempts` in App Config to match your provider's rate limits and cost budget.
