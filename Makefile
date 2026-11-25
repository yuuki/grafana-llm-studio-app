# Thin entry points shared with the maintainer's other repositories. The work
# itself stays in the existing task runners: npm scripts for the frontend and
# the repository scripts, the go tool for the backend, Docker Compose for the
# live stack. `make help` lists the targets; AGENTS.md says which one to run
# when.

# Tool versions. CI pins the same versions (.github/workflows); bump together.
GOLANGCI_LINT_VERSION   ?= v2.15.0
GOVULNCHECK_VERSION     ?= v1.9.0
ACTIONLINT_VERSION      ?= v1.7.12
ZIZMOR_VERSION          ?= 1.32.0
GH_ACTIONS_LOCK_VERSION ?= v0.4.1
OSV_SCANNER_VERSION     ?= v2.7.0
FUZZTIME                ?= 30s
FUZZFLAGS               ?=
BENCH_COUNT             ?= 6
# Grafana version of the live stack: 13.3.1 (React 19) or 12.4.15 (React 18).
GRAFANA_VERSION         ?= 13.3.1

# Not ./...: `npm ci` installs a Go package (flatted) under node_modules.
GO_PACKAGES := ./pkg/... ./cmd/...

# `go run tool@version` would build each tool with that tool's own minimum Go
# version, and golangci-lint refuses to lint code newer than the Go it was
# built with. Build the tools with this module's toolchain (go env GOVERSION
# inside the module resolves to the go.mod go/toolchain line).
GO_TOOLCHAIN = $(shell go env GOVERSION)
GO_RUN = GOTOOLCHAIN=$(GO_TOOLCHAIN) go run

GOLANGCI_LINT ?= $(GO_RUN) github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
# uvx or pipx (preinstalled on GitHub-hosted runners) runs the pinned zizmor.
ZIZMOR ?= $(if $(shell command -v uvx 2>/dev/null),uvx zizmor@$(ZIZMOR_VERSION),pipx run zizmor==$(ZIZMOR_VERSION))
# gh-actions-lock (the `gh actions-lock` extension) runs through `go run` too.
GH_ACTIONS_LOCK ?= $(GO_RUN) github.com/github/gh-actions-lock/cmd/gh-actions-lock@$(GH_ACTIONS_LOCK_VERSION)
OSV_SCANNER ?= $(GO_RUN) github.com/google/osv-scanner/v2/cmd/osv-scanner@$(OSV_SCANNER_VERSION)

COMPOSE := docker compose -f deploy/docker-compose.yml
BACKEND_BINARY := dist/gpx_yuuki_llmstudio_app_linux_amd64

# `npm ci` writes this file, so dependencies are reinstalled only when the lockfile changes.
NODE_MODULES := node_modules/.package-lock.json

# pipefail keeps `go test ... | tee` from hiding a failing run.
SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c

.DEFAULT_GOAL := help
.PHONY: help check verify build build-frontend build-backend build-bench clean \
	lint lint-go lint-frontend vet typecheck fmt lint-actions lock-actions \
	verify-schemas verify-bundle verify-sse verify-llm-protocol \
	test test-go test-race test-ts test-frontend test-e2e test-integration test-fuzz bench \
	vuln vuln-deps vuln-deps-sarif complexity

help: ## List the targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

check: lint lint-actions verify-schemas test-race test-frontend build ## Local pre-PR gate: the required PR CI minus the live E2E stack

verify: verify-schemas vet typecheck test-race test-frontend build ## Schema sync, vet, type check, unit tests and the plugin build (`make check` adds the linters)

build: build-frontend build-backend ## Build the plugin into dist/ (frontend bundle, bundle check, linux/amd64 backend)

build-frontend: $(NODE_MODULES) ## Production frontend bundle (webpack) and its externals check
	npm run build
	npm run verify:bundle

build-backend: ## Backend binary for linux/amd64 (dist/gpx_yuuki_llmstudio_app_linux_amd64)
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o $(BACKEND_BINARY) ./pkg

build-bench: ## llm-bench CLI (bin/llm-bench)
	mkdir -p bin
	go build -o bin/llm-bench ./cmd/llm-bench

clean: ## Remove build outputs
	rm -rf dist bin

lint: lint-go lint-frontend ## All linters: Go and frontend

lint-go: ## golangci-lint and go.mod tidiness
	$(GOLANGCI_LINT) run $(GO_PACKAGES)
	go mod tidy -diff

lint-frontend: typecheck ## TypeScript type check and ESLint
	npm run lint

vet: ## go vet
	go vet $(GO_PACKAGES)

typecheck: $(NODE_MODULES) ## TypeScript type check (src and the Playwright tests)
	npm run typecheck

fmt: $(NODE_MODULES) ## Format Go code and apply ESLint fixes
	$(GOLANGCI_LINT) fmt
	npm run lint:fix

# zizmor's online audits need GH_TOKEN; without one (or with GH_TOKEN= on the
# command line) it runs offline.
# gh-actions-lock checks that .github/workflows/actions.lock pins every
# action the workflows use; with GH_TOKEN it also verifies each pin upstream.
lint-actions: ## Lint (actionlint) and audit (zizmor) the GitHub Actions workflows, and check their lockfile
	$(GO_RUN) github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)
	$(if $(GH_TOKEN),$(ZIZMOR) .,env -u GH_TOKEN $(ZIZMOR) --offline .)
	$(GH_ACTIONS_LOCK) $(if $(GH_TOKEN),--verify,--verify-local)

# Pin the actions in .github/workflows/actions.lock after a `uses:` changed,
# then review the diff. Needs a GitHub token (GH_TOKEN or `gh auth login`).
lock-actions: ## Update .github/workflows/actions.lock after changing a uses: line
	$(GH_ACTIONS_LOCK)

verify-schemas: $(NODE_MODULES) ## JSON Schema <-> TypeScript <-> Go enum sync and go-openai compatibility
	npm run verify:schemas

verify-bundle: $(NODE_MODULES) ## dist/module.js externalizes Grafana's shared modules (run after the frontend build)
	npm run verify:bundle

verify-sse: ## Live CallResource SSE flush and disconnect checks (needs the running stack)
	node scripts/verify-sse-callresource.mjs

verify-llm-protocol: build-bench ## Wire checks through real grafana-llm-app containers (Docker)
	$(COMPOSE) -f deploy/docker-compose.bench-verify.yml up -d
	status=0; \
		LLM_BENCH_BIN=./bin/llm-bench node scripts/verify-grafana-llm-app.mjs || status=$$?; \
		$(COMPOSE) -f deploy/docker-compose.bench-verify.yml down -v; \
		exit $$status

test: test-go test-frontend ## All unit tests: Go and frontend

test-go: ## Go unit tests
	go test $(GO_PACKAGES)

test-race: ## Go unit tests with the race detector
	go test -race -count=1 -cover $(GO_PACKAGES)

test-ts: test-frontend

test-frontend: $(NODE_MODULES) ## Frontend unit and parity tests (Jest)
	npm run test:ci

test-e2e: $(NODE_MODULES) ## Playwright tests against a running stack (GRAFANA_URL, default http://127.0.0.1:3000)
	npm run test:e2e

# The CI E2E job, end to end: build, start the stack, wait for it, run the SSE
# checks and Playwright, and always tear the stack down. Needs Docker and
# Playwright's Chromium (npx playwright install chromium).
test-integration: build $(NODE_MODULES) ## Live E2E on GRAFANA_VERSION (13.3.1 or 12.4.15): Compose stack, SSE checks, Playwright
	GRAFANA_VERSION=$(GRAFANA_VERSION) $(COMPOSE) up -d
	status=0; \
		node scripts/wait-for-stack.mjs && \
		node scripts/verify-sse-callresource.mjs && \
		GRAFANA_VERSION=$(GRAFANA_VERSION) npm run test:e2e || status=$$?; \
		if [ $$status -ne 0 ]; then $(COMPOSE) logs --tail=200; fi; \
		$(COMPOSE) down -v; \
		exit $$status

test-fuzz: ## Run every Go fuzz target for FUZZTIME (FUZZFLAGS=-parallel=2 on shared machines)
	@for pkg in $$(go list $(GO_PACKAGES)); do \
		targets=$$(go test -list '^Fuzz' $$pkg); \
		for f in $$(printf '%s\n' "$$targets" | awk '/^Fuzz/'); do \
			echo "== $$pkg $$f"; \
			go test -run='^$$' -fuzz="^$$f\$$" -fuzztime=$(FUZZTIME) $(FUZZFLAGS) $$pkg; \
		done; \
	done

bench: ## Go benchmarks (compare runs with benchstat)
	go test -run='^$$' -bench=. -benchmem -count=$(BENCH_COUNT) $(GO_PACKAGES) | tee bench.txt

vuln: ## Known vulnerabilities reachable from the Go code (govulncheck)
	$(GO_RUN) golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) $(GO_PACKAGES)

vuln-deps: ## Known vulnerabilities in package-lock.json (osv-scanner; fails on findings)
	$(OSV_SCANNER) scan source --lockfile=package-lock.json

# CI uploads this SARIF file to code scanning, which tracks known findings as
# alerts and flags only the ones a pull request introduces. osv-scanner exits
# 1 when it finds vulnerabilities, but `go run` reports every failure as status
# 1, so a scan (or tool build) that failed is recognised by the missing report.
vuln-deps-sarif: ## osv-scanner results as SARIF in osv-scanner.sarif (fails only on scanner errors)
	@rm -f osv-scanner.sarif; status=0; \
		$(OSV_SCANNER) scan source --lockfile=package-lock.json --format=sarif --output-file=osv-scanner.sarif || status=$$?; \
		if [ $$status -ne 0 ] && [ $$status -ne 1 ]; then exit $$status; fi; \
		test -s osv-scanner.sarif || { echo 'vuln-deps-sarif: osv-scanner wrote no report' >&2; exit 1; }

complexity: ## Cognitive / cyclomatic complexity ranking (needs cccc; settings in cccc.toml)
	cccc --table src pkg cmd

$(NODE_MODULES): package-lock.json
	npm ci
