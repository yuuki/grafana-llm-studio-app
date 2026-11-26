# ADR-002: Auth Boundaries & Grafana OSS Single-Organization MVP Scope

- **Status:** Accepted (2026-11-22)
- **Target Versions:** `Grafana OSS v12.4.15 / v13.3.1`

## Context
Grafana does not forward browser session cookies to App Plugin backends (`CallResource`). Additionally, Grafana OSS does not include Enterprise/Cloud datasource-level RBAC or Label-Based Access Control (LBAC).

## Decision
1. **Frontend User-Session Execution:** Metadata discovery, bounded dry-run validation, `@grafana/scenes` preview, and dashboard saving all execute from the frontend using the logged-in user's Grafana session.
2. **Trusted Backend Caller Identification:** The Go backend relies exclusively on `OrgID` and `User.Login` from Grafana's trusted `backend.PluginConfigFromContext(ctx)` and never trusts `orgId` or `userId` supplied in request bodies (rejected via `DisallowUnknownFields`). Every job operation enforces exact `(OrgID, UserLogin)` owner matching.
3. **Strict Isolation of the `llmclient` Service Account:**
   - `plugin.json` `iam.permissions` requests only `{"action":"plugins.app:access","scope":"plugins:grafana-llm-app"}`.
   - Service accounts created manually in Grafana OSS are assigned the minimum basic role `Viewer` (verified in `pkg/services/pluginsintegration/pluginaccesscontrol/accesscontrol.go`).
   - Because Grafana OSS's `Viewer` basic role also includes organization-wide read/query access, the Go backend structurally guarantees this token is never passed anywhere outside `llmclient`, strictly pins `grafanaUrl` to Grafana's trusted `AppURL` base URL (or operator-pinned `LLMSTUDIO_GRAFANA_URL`, falling back to loopback only when neither is present), binds stored UI secrets to their destination, and disables HTTP redirects.
4. **Metadata & Draft Isolation:**
   - Metric names, `HELP` strings, and label names/values are treated as protected data. In the MVP they remain strictly in browser memory and single-request scope and are never registered in a shared persistent index.
   - Switching organizations, changing datasources, or unmounting the page immediately aborts in-flight requests and discards in-memory drafts and metric catalogs.
5. **Explicit Single-Organization MVP Boundary:**
   - Multi-tenant deployments serving users with differing data-access scopes are explicitly **not** supported by this MVP. Production support for distinct user scopes requires separating Grafana organizations and enforcing real data isolation via upstream datasource credentials, tenant headers, or an authorization proxy.
