.PHONY: verify build build-backend build-frontend test test-go test-ts verify-schemas clean

verify: verify-schemas test build

verify-schemas:
	npm run verify:schemas

test: test-go test-ts

test-go:
	go test -race -v ./pkg/...

test-ts:
	npm run typecheck
	npm run test:ci

build: build-frontend build-backend

build-frontend:
	npm run build

build-backend:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dist/gpx_yuuki_llmstudio_app_linux_amd64 ./pkg

clean:
	rm -rf dist
