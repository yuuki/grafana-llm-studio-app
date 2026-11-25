# Contributing

Thanks for helping improve LLM Dashboard Studio.

- **Bugs and ideas**: open an issue describing what you expected and what
  happened (plugin, Grafana and `grafana-llm-app` versions, the LLM provider
  and the structured-output settings help). Security issues go through
  [SECURITY.md](SECURITY.md) instead.
- **Changes**: open a pull request against `main`. Keep it focused on one
  topic, and include tests for behaviour changes. Use generic names in
  fixtures and examples (`checkout-api`, `prom-main`), never real hosts,
  services or tokens.

## Before you open a pull request

```bash
npm ci
make check
```

`make check` runs the checks of the required pull request CI that work on a
laptop: golangci-lint, the TypeScript type check, ESLint, the GitHub Actions
audit, the JSON Schema sync check, Go tests with the race detector, Jest and
the plugin build with its bundle check. It needs Node.js 26 and Go (version in
`go.mod`; `mise install` installs both) and uv or pipx (for zizmor). Run
`make help` for the other targets (the live E2E stack, fuzzing, the
`grafana-llm-app` wire checks, vulnerability scans); [AGENTS.md](AGENTS.md)
explains when each one is needed and lists the compatibility contracts (IR
schemas, resource routes, settings keys, plugin ID) that must not change
silently.

A material design decision gets an ADR in [`docs/adr/`](docs/adr/).

## Commit messages

Commits, branches and pull request titles follow the Git conventions in
[AGENTS.md](AGENTS.md#git-conventions).
