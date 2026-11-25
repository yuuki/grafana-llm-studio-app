# Security policy

LLM Dashboard Studio runs inside Grafana. Its backend calls `grafana-llm-app`
with a dedicated service account token, keeps generation jobs per Grafana
user (in memory or in Redis), and turns LLM output into dashboards that users
preview and save. Issues that let a Grafana user read, change or cancel
another user's jobs, or reach another organization's; extract or misuse the
service account token or the Redis password (for example by pointing
`grafanaUrl` elsewhere); get a query, a datasource UID, HTML or script past
the IR and query validators through a prompt, metric metadata or LLM output;
overwrite a dashboard without asking; or disrupt the plugin backend with
crafted requests are treated as vulnerabilities. So are weaknesses in how the
release archive is built.

Out of scope: the Docker Compose development stack and its default
credentials, the development mock LLM mode, the cost or the quality of what a
configured LLM generates, the limits of Grafana OSS described in
[docs/architecture.md](docs/architecture.md) (one organization shares one data
scope), and the security of Grafana, `grafana-llm-app`, the LLM providers,
Redis, Prometheus or VictoriaMetrics themselves (please report those
upstream).

## Supported versions

Security fixes are made on the `main` branch and released in the next
version. Only the latest release is supported.

## Reporting a vulnerability

Please do **not** open a public issue, pull request or discussion.

Report the vulnerability privately with GitHub's
[private vulnerability reporting](https://github.com/yuuki/grafana-llm-studio-app/security/advisories/new)
(the "Report a vulnerability" button of the Security tab). If that is not
possible, contact the author privately via the contact information on
https://github.com/yuuki.

Please include the plugin version, the Grafana and `grafana-llm-app` versions,
the relevant plugin configuration (with secrets removed), what an attacker
controls (a Grafana role, a prompt, metric names or labels, the LLM's
response, ...), and the impact. You should receive an answer within a few
weeks; the plugin is maintained on a best-effort basis.
