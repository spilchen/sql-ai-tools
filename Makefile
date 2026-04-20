BINARY                  := crdb-sql
BIN_DIR                 := bin
BIN                     := $(BIN_DIR)/$(BINARY)
GO_PKGS                 := ./...
GOFMT_FILES             := $(shell find . -name '*.go' -not -path './vendor/*')

# Pinned tool versions. Bump deliberately; do not float.
GOLANGCI_LINT_VERSION   := v2.11.4
GOLANGCI_LINT           := $(BIN_DIR)/golangci-lint

.DEFAULT_GOAL := help

.PHONY: help build test fmt fmt-check vet lint clean tools

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

lint: fmt-check vet $(GOLANGCI_LINT) ## Run gofmt check, go vet, and pinned golangci-lint (CI gate).
	$(GOLANGCI_LINT) run $(GO_PKGS)

clean: ## Remove build artifacts and installed tools.
	rm -rf $(BIN_DIR)
