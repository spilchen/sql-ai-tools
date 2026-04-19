# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Coding Guidelines

### Engineering Standards

Key concepts and abstractions should be explained clearly, and lifecycles and
ownership clearly stated. Whenever possible, you should use examples to make the
code accessible to the reader. Comments should always add depth to the code
(rather than repeating the code).

When reviewing, other than technical correctness, you should also focus on the
above aspects. Do not over-emphasize on grammar and comment typos, prefix with
"nit:" in reviews.

### Resources

- **Design Documents**: `/docs/`

### When generating PRs and commit records

Use the `commit-helper` skill (invoked via `/commit-helper`) when creating commits and PRs.

- For multi-commit PRs, summarize each commit in the PR record.
- Do not include a test plan unless explicitly asked by the user.

## Build, Test, Lint

Prerequisites: Go 1.26+ (Go's `toolchain` directive will auto-download 1.26.2
if your local Go is newer-major-compatible). `golangci-lint` is pinned and
auto-installed into `bin/` by the Makefile — do not rely on a system install.

All workflows go through the Makefile:

- `make build` — compile to `bin/sql-ai-tools`
- `make test` — run the Go test suite (`go test ./...`)
- `make fmt` — auto-format sources with gofmt
- `make fmt-check` — fail if any file is not gofmt-clean
- `make vet` — run `go vet ./...`
- `make tools` — install pinned dev tools into `bin/` (currently `golangci-lint`)
- `make lint` — run `fmt-check`, `vet`, then pinned `golangci-lint run` (the CI gate)
- `make clean` — remove `bin/` (binary and installed tools)

The pinned `golangci-lint` version lives in the `Makefile` as
`GOLANGCI_LINT_VERSION`. Bump it deliberately and commit; do not float.

Formatting policy: `go fmt` violations do **not** fail `make build` — formatting
is treated as a separate concern from compilation, matching standard Go-community
practice (Kubernetes, CockroachDB, etcd, Prometheus all do this). They **do**
fail `make lint`, which is what CI runs. Configure your editor to run
`gofmt`/`goimports` on save so violations never reach CI.

There is no git pre-commit hook; `make lint` is the single source of truth.

# Interaction Style

* Be direct and honest.
* Skip unnecessary acknowledgments.
* Correct me when I'm wrong and explain why.
* Suggest better alternatives if my ideas can be improved.
* Focus on accuracy and efficiency.
* Challenge my assumptions when needed.
* Prioritize quality information and directness.
