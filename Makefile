.PHONY: test lint check sync-db build build-static package dist dist-target e2e

VERSION ?= $(shell awk -F '"' '/"version"/{print $$4; exit}' plugin.json)
TAG ?= v$(VERSION)
DIST ?= .tmp/dist
PACKAGE ?= .tmp/package

test: ## Unit and contract tests
	go test ./...

lint: ## Format and vet
	@test -z "$$(gofmt -l .)" || (echo "gofmt pending:"; gofmt -l .; exit 1)
	go vet ./...

sync-db: ## Refresh source-tree checksums.txt for plugin.json
	shasum -a 256 plugin.json > checksums.txt

build: ## Nerve, chart, Scribe, and watcher (native FSEvents on macOS)
	mkdir -p .tmp
	go build -o .tmp/roca-firstmate ./cmd/roca-firstmate

build-static: ## Portable build with polling watcher fallback
	mkdir -p .tmp
	CGO_ENABLED=0 go build -o .tmp/roca-firstmate-static ./cmd/roca-firstmate

package: build-static ## Installable plugin directory for the host platform
	go run ./cmd/package --binary .tmp/roca-firstmate-static --out $(PACKAGE) --version $(VERSION)

dist: ## darwin-arm64 and linux-amd64 release archives
	mkdir -p $(DIST)
	$(MAKE) dist-target GOOS=darwin GOARCH=arm64 TARGET=darwin-arm64
	$(MAKE) dist-target GOOS=linux GOARCH=amd64 TARGET=linux-amd64

dist-target:
	mkdir -p $(DIST) .tmp
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -o .tmp/roca-firstmate-$(TARGET) ./cmd/roca-firstmate
	env -u GOOS -u GOARCH go run ./cmd/package --binary .tmp/roca-firstmate-$(TARGET) --out .tmp/package-$(TARGET) --version $(VERSION) \
		--archive $(DIST)/roca-firstmate-$(TAG)-$(TARGET).tar.gz
	cp $(DIST)/roca-firstmate-$(TAG)-$(TARGET).tar.gz $(DIST)/roca-firstmate-$(TARGET).tar.gz

e2e: ## Scratch-home install, dispatch, and release-to-release update
	ROCA_E2E=1 go test ./internal/release -count=1 -timeout 180s

check: lint test ## CI gate
