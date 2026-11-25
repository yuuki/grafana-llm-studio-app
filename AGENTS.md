# AGENTS.md

## Rules
- `docs/architecture.md` and the ADRs (`docs/adr/`) record the design and its reasons: read the ones for what you change; record a material decision in a new ADR.
- For Grafana plugin APIs follow https://grafana.com/developers/plugin-tools/, not training data, which is out of date.
- Run Go commands on `./pkg/... ./cmd/...`, never `./...`: `npm ci` installs a Go package (flatted) under `node_modules/`.
- LLM traffic goes only backend → `llmclient` → `grafana-llm-app`: no direct LLM connection, no browser-side `@grafana/llm`, no silent fallback to mock mode.
- The service account token is used only by `llmclient`: `grafanaUrl` stays loopback or the AppURL origin, redirects stay off; datasource queries, preview and saving run in the browser with the user's session.
- The backend trusts only `OrgID` and `User.Login` of the `PluginContext`; request bodies are decoded with `DisallowUnknownFields`, and every job operation checks the `(OrgID, UserLogin)` owner.
- Metric names, `HELP` texts and label values stay in browser memory: no server-side metadata cache or index shared across users.
- LLM output is untrusted: it reaches a draft only through `DecodeStrictIR`/`DecodeStrictPatch`, `ValidateIR` and the query validator (AST, catalog, scope); never raw dashboard JSON.
- JSON Schemas use string-only `enum` and no `oneOf`/`anyOf` (`grafana-llm-app` relays them through go-openai); bounds the relay drops are re-checked in `pkg/ir`.
- Transport retries (at most 1, for 502/503/504 and upstream errors wrapped in a 500) stay separate from repairs (at most 2 per job); 429 and other 4xx are never retried.
- Saving is create-only (`POST` of `dashboard.grafana.app/v1beta1`, a new UID, no `PUT` after a 409); the opt-in update of an imported dashboard needs its `resourceVersion`.
- Preview and saved JSON come from one `NormalizedDashboardSpec` (`src/spec/normalize.ts`), kept equal by `src/spec/parity.test.ts`.
- The webpack externals follow Grafana 12.4's and 13.3's shared modules (comment in `webpack.config.js`), checked by `npm run verify:bundle`.
- Errors wrap only their sentinel with `%w` and format the cause with `%v` (`"%w: ...: %v"`), so `errors.Is` sees the classification, not a Redis or regexp error.
- Every UI string goes into both `src/i18n/locales/en.json` and `ja.json` with the same placeholders.

## Compatibility contracts
- Plugin ID, type and backend executable (`src/plugin.json`, `pkg/main.go`): never change; `grafanaDependency` and `iam.permissions` are the support and permission promise.
- Supported versions, changed together: Grafana 13.3.1 and 12.4.15, `grafana-llm-app` v1.2.0 and v1.0.8, `grafana-plugin-sdk-go`, `@grafana/*` 12.4 (`docs/compatibility.md`, `deploy/`, the CI matrix).
- IR and patch format, versions 1.0 and 1.1: `schemas/*.schema.json` ↔ `src/ir/types.ts` ↔ `pkg/ir` (`npm run verify:schemas`); drafts and imports of older versions keep decoding.
- Resource routes and their JSON (`/health`, `/config/public`, `/jobs/generate`, `/jobs/import`, `/jobs/{id}` and its `patch`, `repair`, `undo`, `cancel`): `pkg/plugin/app.go` ↔ `src/api/studioClient.ts`; SSE events `stage`, `result`, `error` (`pkg/plugin/sse.go`).
- Settings `jsonData`/`secureJsonData` keys, defaults and bounds: `pkg/plugin/settings.go` ↔ `src/components/AppConfig/`, the README table; `PublicConfig` never carries a secret.
- Redis job records and keys under `{redisKeyPrefix}` (`pkg/jobs/redis_store.go`): replicas of two versions share them during a rolling upgrade.
- Saved and exported dashboard JSON (`src/spec/toFoundation.ts`, `src/save/saveDashboard.ts`); `llm-bench` flags and report (`cmd/llm-bench`, `pkg/bench`).
- Release archive `yuuki-llmstudio-app-<version>.zip` (`release.yml`); the tag matches `version` in `package.json` and `src/plugin.json`.

## Before opening a pull request
- `npm ci`, then `make check`; also `make test-integration` with `GRAFANA_VERSION=13.3.1` and `12.4.15` (Docker) for pages, routes, SSE, `deploy/` or `tests/`.
- Decoders, validators, schemas or settings parsing: also `make test-fuzz`; dependencies: also `make vuln vuln-deps`.
- `pkg/llm`, prompts or structured-output settings: also `make verify-llm-protocol` (real `grafana-llm-app` containers).
- `src/plugin.json`: restart Grafana (it reads the file only at start) and say so in the PR.

## Git conventions
- Branches: `<type>/<issue>-<summary>` (`<type>/<summary>` without an issue); tools' `agent/*`, `claude/*`, `copilot/*`, `dependabot/*` keep their names.
- Commits and PR titles: Conventional Commits 1.0.0, types `feat` `fix` `perf` `refactor` `test` `docs` `build` `ci` `chore` `revert`; scopes: `ir`, `promql`, `metricsql`, `jobs`, `llm`, `sse`, `settings`, `studio`, `save`, `import`, `i18n`, `bench`, `deploy`.
