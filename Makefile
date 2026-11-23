.PHONY: verify build build-backend build-frontend test test-go test-ts test-e2e verify-schemas verify-bundle clean

verify: verify-schemas test build

verify-schemas:
	npm run verify:schemas

verify-bundle:
	npm run verify:bundle

test: test-go test-ts

test-go:
	go vet ./pkg/...
	go test -race -count=1 -v ./pkg/...

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

clean:
	rm -rf dist
