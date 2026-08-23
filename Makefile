.PHONY: test lint check sync-db build build-static

test: ## Unit and contract tests
	go test ./...

lint: ## Format and vet
	@test -z "$$(gofmt -l .)" || (echo "gofmt pending:"; gofmt -l .; exit 1)
	go vet ./...

sync-db: ## Refresh checksums.txt for the shipped plugin.json payload
	shasum -a 256 plugin.json > checksums.txt

build: ## Nerve, chart, Scribe, and watcher (native FSEvents on macOS)
	mkdir -p .tmp
	go build -o .tmp/roca-firstmate ./cmd/roca-firstmate

build-static: ## Portable build with polling watcher fallback
	mkdir -p .tmp
	CGO_ENABLED=0 go build -o .tmp/roca-firstmate-static ./cmd/roca-firstmate

check: lint test ## CI gate
