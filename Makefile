.PHONY: verify build build-backend build-backend-all build-frontend build-bench test test-go test-ts test-release test-e2e verify-schemas verify-bundle verify-package verify-llm-protocol verify-sse sign package package-airgap clean

verify: verify-schemas test build

verify-schemas:
	npm run verify:schemas

verify-bundle:
	npm run verify:bundle

test: test-go test-ts test-release

test-go:
	go vet ./pkg/... ./cmd/...
	go test -race -count=1 -v ./pkg/... ./cmd/...

test-ts:
	npm run typecheck
	npm run test:ci

test-release:
	npm run test:release

test-e2e:
	npm run test:e2e

build: build-frontend build-backend

build-frontend:
	npm run build
	npm run verify:bundle

build-backend:
	node scripts/build-backend.mjs --targets linux/amd64

build-backend-all:
	node scripts/build-backend.mjs --all

sign:
	npm run sign

package: build-frontend build-backend-all
	node scripts/package-release.mjs --sign auto
	node scripts/verify-release.mjs

package-airgap: build-frontend build-backend-all
	node scripts/package-release.mjs --sign auto --airgap
	node scripts/verify-release.mjs --airgap

verify-package:
	node scripts/verify-release.mjs

build-bench:
	mkdir -p bin
	go build -o bin/llm-bench ./cmd/llm-bench

verify-llm-protocol: build-bench
	docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.bench-verify.yml up -d
	LLM_BENCH_BIN=./bin/llm-bench node scripts/verify-grafana-llm-app.mjs; \
	status=$$?; \
	docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.bench-verify.yml down -v; \
	exit $$status

verify-sse:
	node scripts/verify-sse-callresource.mjs

clean:
	rm -rf dist bin release
