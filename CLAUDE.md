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

### TODO Comments

Every TODO in source code **must** reference an open GitHub issue in this
repo using the form `TODO(#123)` (e.g. `// TODO(#42): handle NULL casts`).
No other form is allowed — not `TODO`, not `TODO(name)`, not `FIXME`, not
`XXX`.

If no open issue covers the work, create one first using the
`issue-create` skill, then use that issue's number in the TODO. Don't
leave a TODO without a tracking issue.

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

## Worktree Workflow

Parallel feature work happens in ephemeral git worktrees driven by Claude
Code's native `--worktree` flag. Each worktree maps 1:1 to a GitHub issue
and is expected to produce exactly one PR before being torn down.

Worktrees live at `.claude/worktrees/<slug>/` (gitignored). Each gets its
own `bin/`, so builds don't collide across parallel sessions.

Branch name format (date is `YYMMDD`, time is 12-hour `HHMM` + `a`/`p`):

    <gh-username>/gh-<issue#>/<YYMMDD>/<HHMM[a|p]>/<slug>
    e.g. spilchen/gh-42/260419/0245p/validate-sql  # 2026-04-19, 2:45pm

The slashes are real ref hierarchy — they group cleanly in `gh pr list`
and the GitHub UI. The timestamp prevents collisions when reopening the
same issue under a new slug.

Helpers in `scripts/`:

- `scripts/wt-new -i <issue#> -s <slug>` — verifies the issue exists,
  computes the branch, and execs `claude --worktree <slug>`. The
  `WorktreeCreate` hook in `.claude/settings.json` creates the actual
  branch off `origin/HEAD`. Note: `origin/HEAD` is a *local* symbolic
  ref — no `git fetch` runs, so the worktree starts from whatever
  origin pointed at the last time you fetched. Run `git fetch` first
  if you need the freshest base.
- `scripts/wt-ls` — list worktrees with PR state (OPEN/MERGED/CLOSED/
  NONE/DIRTY/MISSING/GH-ERR/STAT-ERR). MISSING means the worktree
  directory is gone; GH-ERR means the `gh` lookup failed; STAT-ERR
  means `git status` failed (likely a corrupt worktree).
- `scripts/wt-prune` — query `gh` for each worktree's branch; remove
  worktree + branch if its PR has merged. Skips dirty worktrees unless
  `--force`. Use `--dry-run` first if unsure.

Direct `claude --worktree <slug>` (without `wt-new`) still works — the
hook falls back to a `wt/<slug>` branch.

Make wrappers: `make wt-new ISSUE=42 SLUG=foo`, `make wt-ls`,
`make wt-prune`.

# Interaction Style

* Be direct and honest.
* Skip unnecessary acknowledgments.
* Correct me when I'm wrong and explain why.
* Suggest better alternatives if my ideas can be improved.
* Focus on accuracy and efficiency.
* Challenge my assumptions when needed.
* Prioritize quality information and directness.
