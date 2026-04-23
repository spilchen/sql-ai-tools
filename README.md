# sql-ai-tools

Agent-friendly SQL tooling for CockroachDB. An MCP server and CLI that wraps
CockroachDB's parser, type system, and error infrastructure in a structured,
agent-consumable interface.

## How this fits with `cockroachdb/claude-plugin`

`sql-ai-tools` is the **offline, parser-grade validation layer**: parse,
type-check, and name-resolve CRDB SQL without a cluster, with structured
fix-suggestion errors. It complements
[`cockroachdb/claude-plugin`](https://github.com/cockroachdb/claude-plugin),
which is the **distribution and execution layer** — a Claude-Code plugin
bundling sub-agents, skills, hooks, and proxied MCP backends (MCP Toolbox,
CockroachDB Cloud MCP) for live cluster work.

The two projects are complementary, not competing: `claude-plugin` requires
a live cluster for every tool; `sql-ai-tools` works on a plane. See
[`docs/claude_plugin_comparison.md`](docs/claude_plugin_comparison.md) for
the full breakdown.

## Problem

AI agents write SQL for CockroachDB constantly — and get it wrong in subtle
ways. The current workflow is:

```
Today:     write SQL → execute → parse error string → guess fix → retry (×5)
With tool: write SQL → validate_sql → read JSON → apply fix → execute (×1)
```

CockroachDB has a world-class SQL parser, type system, and structured error
infrastructure — but these capabilities are trapped inside the server binary.
This project exposes them as standalone, agent-friendly operations.

## Features

**MCP tools and CLI commands** for the full SQL lifecycle:

| Tool | Description | Tier |
|------|-------------|------|
| `validate_sql` | Structured error list with positions, suggestions, available names | 1-2 |
| `format_sql` | Canonicalized SQL with optional syntax highlighting | 1 |
| `parse_sql` | Statement type, tag, fingerprint, parser version | 1 |
| `list_tables` | Table names from loaded schema or live catalog | 2-3 |
| `describe_table` | Columns, types, constraints, indexes | 2-3 |
| `explain_sql` | EXPLAIN output as structured JSON | 3 |
| `explain_schema_change` | Schema changer plan with phases and operations | 3 |
| `detect_risky_query` | Risk assessment with reason codes and fix hints | 1-3 |

**Three-tier progressive capability:**

- **Tier 1 — Zero-config:** Parse, format, classify, type-check expressions.
  Works offline with no setup.
- **Tier 2 — Schema files:** Load CREATE TABLE files for name resolution,
  column type validation, and "did you mean?" suggestions. No cluster needed.
- **Tier 3 — Connected:** EXPLAIN, schema change analysis, and guarded
  execution against a live CockroachDB cluster.

**Structured JSON error output:**

```json
{
  "errors": [{
    "code": "42703",
    "message": "column \"nme\" does not exist",
    "position": {"line": 1, "column": 8},
    "category": "unknown_column",
    "available": ["name", "email", "id"],
    "suggestions": [{"replacement": "name", "confidence": 0.9}]
  }]
}
```

## Architecture

Built on [cockroachdb-parser](https://github.com/cockroachdb/cockroachdb-parser),
the standalone extraction of CockroachDB's SQL parser. This gives exact parser
fidelity with CockroachDB — including CRDB-specific syntax like hash-sharded
indexes, regional tables, and PL/pgSQL — in a lightweight Go module.

Key components from the parser module:

- **Parser** — `Parse()`, `ParseOne()`, `ParseExpr()`, `Tokens()`
- **AST** — `Visitor`/`ExtendedVisitor` for tree walking, `PrettyCfg` for formatting
- **Type system** — All SQL types, OID mapping, casting rules (fully self-contained)
- **Error handling** — `pgerror` with SQLSTATE codes, severity, hint, detail

Semantic analysis is built above the parser extraction: expression type checking
via `MakeSemaContext(nil)`, name resolution against an in-memory schema catalog,
and function validation against the builtins registry.

## Getting Started

### Prerequisites

- Go 1.26+ (Go's `toolchain` directive will auto-download the matching
  toolchain on first build if your local install is older but compatible)

### Build

```bash
make build
```

Produces `bin/crdb-sql`.

Alternatively, install the latest-quarter binary directly with `go install`:

```bash
go install github.com/spilchen/sql-ai-tools/cmd/crdb-sql@latest
crdb-sql version
```

`go install` produces only the unsuffixed `crdb-sql` (latest quarter)
without the per-quarter `crdb-sql-vXXX` siblings shipped in the release
archives. Passing `--target-version` for a different quarter without
the matching sibling on `$PATH` is a hard error: `crdb-sql` prints a
discovery hint to stderr and exits with status 2 (no silent fallback
to the wrong parser). Install the matching `crdb-sql-vXXX` from a
release archive when you need an older quarter.

### Run

```bash
# Show available subcommands
./bin/crdb-sql --help

# Print binary and parser versions
./bin/crdb-sql version
# crdb-sql: dev
# cockroachdb-parser: v0.26.2
```

### Use as an MCP server

`crdb-sql mcp` runs the binary as a Model Context Protocol server over
stdio. The current build only registers a `ping` health-check tool;
real SQL tools (`validate_sql`, `format_sql`, …) listed in the
[Features](#features) table land in subsequent issues.

Register the binary with Claude Code:

```bash
claude mcp add crdb-sql -- "$(pwd)/bin/crdb-sql" mcp
```

The leading `--` is required so the `mcp` argument is forwarded to
`crdb-sql` instead of being parsed by the `claude` CLI. No transport
flags are needed — `claude mcp add` defaults to stdio, which is what
this server speaks.

Verify discovery from inside Claude Code:

```
/mcp
```

`crdb-sql` should appear in the list with its `ping` tool. Calling
`ping` returns:

```json
{"ok": true, "parser_version": "v0.26.2"}
```

The `parser_version` value should match the `cockroachdb-parser:` line
from `./bin/crdb-sql version`.

### Test & Lint

```bash
make test    # go test ./...
make lint    # gofmt check + go vet + golangci-lint (CI gate)
make fmt     # auto-format sources
```

`make lint` is the CI gate. `go fmt` violations do not block `make build`,
so configure your editor to run `gofmt`/`goimports` on save.

## Project Status

Early development. See [docs/](docs/) for the design document, hackathon plan,
and research lessons that inform the architecture.

## License

Apache License 2.0. See [LICENSE](LICENSE) for details.
