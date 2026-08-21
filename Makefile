.PHONY: test lint check sync-db build build-static

test: ## Unit and contract tests
	go test ./...

lint: ## Format and vet
	@test -z "$$(gofmt -l .)" || (echo "gofmt pending:"; gofmt -l .; exit 1)
	go vet ./...

sync-db: ## Rebuild firstmate.db from schema.sql and refresh checksums.txt
	rm -f firstmate.db firstmate.db-journal firstmate.db-wal firstmate.db-shm
	sqlite3 firstmate.db < schema/schema.sql
	shasum -a 256 plugin.json firstmate.db > checksums.txt

build: ## Nerve, chart, Scribe, and watcher (native FSEvents on macOS)
	mkdir -p .tmp
	go build -o .tmp/roca-firstmate ./cmd/roca-firstmate

build-static: ## Portable build with polling watcher fallback
	mkdir -p .tmp
	CGO_ENABLED=0 go build -o .tmp/roca-firstmate-static ./cmd/roca-firstmate

check: lint test ## CI gate
