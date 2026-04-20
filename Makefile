BINARY                  := crdb-sql
BIN_DIR                 := bin
BIN                     := $(BIN_DIR)/$(BINARY)
GO_PKGS                 := ./...
GOFMT_FILES             := $(shell find . -name '*.go' -not -path './vendor/*')

# Pinned tool versions. Bump deliberately; do not float.
GOLANGCI_LINT_VERSION   := v2.11.4
GOLANGCI_LINT           := $(BIN_DIR)/golangci-lint

.DEFAULT_GOAL := help

.PHONY: help build test fmt fmt-check vet lint clean tools tidy-check

help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Targets:\n"} /^[a-zA-Z_-]+:.*?##/ {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Compile the binary into bin/.
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN) .

test: ## Run the Go test suite.
	go test $(GO_PKGS)

fmt: ## Auto-format sources with gofmt.
	gofmt -w $(GOFMT_FILES)

fmt-check: ## Fail if any source file is not gofmt-clean.
	@out=$$(gofmt -l $(GOFMT_FILES)); \
	if [ -n "$$out" ]; then \
		echo "gofmt needed on:"; echo "$$out"; \
		exit 1; \
	fi

vet: ## Run go vet.
	go vet $(GO_PKGS)

# `go mod tidy -diff` writes the diff to stdout and exits 1 when go.mod /
# go.sum need updating (its documented "tidy needed" signal). It also
# exits non-zero on real tool failures (network outage, proxy 5xx,
# unknown flag), but those write to stderr with empty stdout. Capture
# stdout and stderr separately so we can route the actionable
# "run go mod tidy" message to the common case and reserve "failed" for
# genuine tool errors. Merging the streams (`2>&1`) would conflate the
# two and either hide the remediation hint or rubber-stamp a real failure.
tidy-check: ## Fail if go.mod / go.sum are not tidy.
	@errfile=$$(mktemp) || { echo "tidy-check: mktemp failed"; exit 1; }; \
	trap 'rm -f "$$errfile"' EXIT INT TERM; \
	diff=$$(go mod tidy -diff 2>"$$errfile"); rc=$$?; \
	err=$$(cat "$$errfile"); \
	if [ -n "$$diff" ]; then \
		echo "go mod tidy would change files:"; echo "$$diff"; \
		echo "Run 'go mod tidy' and commit the result."; \
		exit 1; \
	fi; \
	if [ $$rc -ne 0 ]; then \
		echo "go mod tidy -diff failed (exit $$rc):"; echo "$$err"; \
		exit $$rc; \
	fi

# Install or refresh the pinned golangci-lint into bin/.
# Re-installs if the binary is missing or its version doesn't match GOLANGCI_LINT_VERSION.
tools: $(GOLANGCI_LINT) ## Install pinned dev tools into bin/.

$(GOLANGCI_LINT):
	@mkdir -p $(BIN_DIR)
	@if [ ! -x $(GOLANGCI_LINT) ] || ! $(GOLANGCI_LINT) version 2>/dev/null | grep -q "$(GOLANGCI_LINT_VERSION:v%=%)"; then \
		echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION) into $(BIN_DIR)/..."; \
		curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh \
			| sh -s -- -b $(BIN_DIR) $(GOLANGCI_LINT_VERSION); \
	fi

lint: fmt-check vet tidy-check $(GOLANGCI_LINT) ## Run gofmt check, go vet, tidy check, and pinned golangci-lint (CI gate).
	$(GOLANGCI_LINT) run $(GO_PKGS)

clean: ## Remove build artifacts and installed tools.
	rm -rf $(BIN_DIR)
