BINARY                  := crdb-sql
BIN_DIR                 := bin
BIN                     := $(BIN_DIR)/$(BINARY)
GO_PKGS                 := ./...
GOFMT_FILES             := $(shell find . -name '*.go' -not -path './vendor/*')

# Pinned tool versions. Bump deliberately; do not float.
GOLANGCI_LINT_VERSION   := v2.11.4
GOLANGCI_LINT           := $(BIN_DIR)/golangci-lint

.DEFAULT_GOAL := help

# Builtin stub generation. Only needed when cockroachdb-parser is bumped.
COCKROACH_SRC           ?= /mnt/scratch/git/cockroach-3
STUBS_VERSION           ?= v26.2
BUILTINS_JSON           := internal/builtinstubs/testdata/crdb_builtins_$(STUBS_VERSION).json
BUILTINS_GEN            := internal/builtinstubs/stubs_$(subst .,_,$(STUBS_VERSION))_gen.go

# Multi-quarter build matrix.
#
# LATEST_QUARTER is the CRDB Year.Quarter the default `make build`
# target compiles against. It is stamped into the binary via the
# versionroute.builtQuarterStamp ldflag so the routing logic in
# cmd/crdb-sql/main.go knows which sibling to delegate to when
# --target-version requests a different quarter. Bump it (and add
# a build/go.vXXX.mod / stubs file) when a newer parser fork is
# the new default.
#
# QUARTERS lists every supported per-quarter backend. `make build-all`
# walks this list. Adding an entry here without a matching
# build/go.vXXX.mod and internal/builtinstubs/stubs_vN_M_gen.go will
# fail the per-quarter target — see build/parser-versions.yaml for
# the new-quarter checklist.
LATEST_QUARTER          := v262
QUARTERS                := v262
ROUTE_PKG               := github.com/spilchen/sql-ai-tools/internal/versionroute
LDFLAGS_LATEST          := -X $(ROUTE_PKG).builtQuarterStamp=$(LATEST_QUARTER)

.PHONY: help build build-all build-latest test test-integration fmt fmt-check vet lint clean tools tidy-check generate-builtins

help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Targets:\n"} /^[a-zA-Z_-]+:.*?##/ {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Compile the latest-quarter binary into bin/crdb-sql.
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS_LATEST)" -o $(BIN) ./cmd/crdb-sql

# Pattern target: build-vXXX produces bin/crdb-sql-vXXX from the
# matching build/go.vXXX.mod (a copy of the top-level go.mod with its
# `replace` directive pointing at the per-quarter parser fork tag).
# Both the modfile and a corresponding stubs file in
# internal/builtinstubs must exist; see build/parser-versions.yaml.
#
# Special case: when XXX matches LATEST_QUARTER, no -modfile is needed
# (the top-level go.mod already pins the latest fork tag). The pattern
# rule covers both cases by testing for the build/go.vXXX.mod file.
build-v%:
	@mkdir -p $(BIN_DIR)
	@modfile=build/go.v$*.mod; \
	if [ -f "$$modfile" ]; then \
		echo "go build -modfile=$$modfile -o $(BIN_DIR)/$(BINARY)-v$* ./cmd/crdb-sql"; \
		go build -modfile="$$modfile" -ldflags "-X $(ROUTE_PKG).builtQuarterStamp=v$*" -o $(BIN_DIR)/$(BINARY)-v$* ./cmd/crdb-sql; \
	elif [ "v$*" = "$(LATEST_QUARTER)" ]; then \
		echo "go build (latest, no modfile) -o $(BIN_DIR)/$(BINARY)-v$* ./cmd/crdb-sql"; \
		go build -ldflags "-X $(ROUTE_PKG).builtQuarterStamp=v$*" -o $(BIN_DIR)/$(BINARY)-v$* ./cmd/crdb-sql; \
	else \
		echo "build-v$*: missing build/go.v$*.mod and v$* != LATEST_QUARTER ($(LATEST_QUARTER))"; \
		echo "Add build/go.v$*.mod (see build/parser-versions.yaml) or fix the quarter tag."; \
		exit 2; \
	fi

# build-latest is an alias for the unsuffixed `make build` so the
# release matrix can refer to all quarters uniformly.
build-latest: build ## Alias for `make build` — compile the latest-quarter binary.

build-all: $(addprefix build-, $(QUARTERS)) build ## Compile every supported per-quarter backend plus the unsuffixed latest.

test: ## Run the Go test suite.
	go test $(GO_PKGS)

# Integration tests are gated behind the `integration` build tag and
# require a real cockroach binary. Set COCKROACH_BIN to override the
# default $PATH lookup. With neither COCKROACH_BIN nor CRDB_TEST_DSN
# set, the tests t.Skip cleanly so this target is safe to run on
# machines that have not installed cockroach.
test-integration: ## Run integration tests (requires cockroach binary; set COCKROACH_BIN to override).
	go test -tags integration -count=1 -timeout 180s $(GO_PKGS)

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

generate-builtins: $(BUILTINS_JSON) ## Generate Go stubs from JSON catalog.
	go run ./cmd/gen-builtins \
		-input=$(BUILTINS_JSON) \
		-output=$(BUILTINS_GEN) \
		-version=$(STUBS_VERSION)
	gofmt -w $(BUILTINS_GEN)

clean: ## Remove build artifacts and installed tools.
	rm -rf $(BIN_DIR)
