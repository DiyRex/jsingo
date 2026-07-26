SHELL := /bin/bash
.DEFAULT_GOAL := check

GO      ?= go
PKGS    := ./...
FUZZTIME ?= 30s

.PHONY: check
check: fmt vet lint test ## fmt, vet, lint and test

.PHONY: fmt
fmt: ## format and fail if anything changed
	@out=$$($(GO) fmt $(PKGS)); \
	if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet:
	$(GO) vet $(PKGS)

.PHONY: lint
lint: ## golangci-lint, skipped with a warning if absent
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed - skipping (brew install golangci-lint)"; \
	fi

.PHONY: test
test: ## unit tests with race detector
	$(GO) test -race -count=1 $(PKGS)

.PHONY: cover
cover:
	$(GO) test -race -coverprofile=coverage.out -covermode=atomic $(PKGS)
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: fuzz
fuzz: ## run every fuzz target for FUZZTIME
	$(GO) test ./internal/wire -run='^$$' -fuzz=FuzzDecodeFrame -fuzztime=$(FUZZTIME)
	$(GO) test ./internal/wire -run='^$$' -fuzz=FuzzDecodeCall  -fuzztime=$(FUZZTIME)

.PHONY: integration
integration: ## needs a real bun or node on PATH
	$(GO) test -race -count=1 -tags=integration ./integration/...

.PHONY: bench
bench:
	$(GO) test -run='^$$' -bench=. -benchmem ./internal/wire

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: clean
clean:
	rm -rf bin dist coverage.out

.PHONY: help
help:
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n",$$1,$$2}'

.PHONY: host
host: ## rebuild the embedded JS host from jsruntime/
	cd jsruntime && bun install --frozen-lockfile --ignore-scripts
	bun build jsruntime/src/main.ts --target=bun --minify \
		--outfile=internal/hostsrc/dist/host.js
	@echo "rebuilt internal/hostsrc/dist/host.js"

.PHONY: host-check
host-check: ## fail if the committed host bundle is stale
	@cp internal/hostsrc/dist/host.js /tmp/host.committed.js
	@$(MAKE) -s host >/dev/null
	@if ! cmp -s /tmp/host.committed.js internal/hostsrc/dist/host.js; then \
		echo "internal/hostsrc/dist/host.js is stale; run 'make host' and commit"; \
		exit 1; \
	fi
	@echo "embedded host bundle is current"
