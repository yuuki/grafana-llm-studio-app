.PHONY: verify build build-backend build-frontend build-bench test test-go test-ts test-e2e verify-schemas verify-bundle verify-llm-protocol verify-sse clean

verify: verify-schemas test build

verify-schemas:
	npm run verify:schemas

verify-bundle:
	npm run verify:bundle

test: test-go test-ts

test-go:
	go vet ./pkg/... ./cmd/...
	go test -race -count=1 -v ./pkg/... ./cmd/...

test-ts:
	npm run typecheck
	npm run test:ci

test-e2e:
	npm run test:e2e

build: build-frontend build-backend

build-frontend:
	npm run build
	npm run verify:bundle

build-backend:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dist/gpx_yuuki_llmstudio_app_linux_amd64 ./pkg

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
	rm -rf dist bin
