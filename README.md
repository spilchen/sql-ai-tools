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

## Tool catalog

Every tool is exposed as both a CLI subcommand (`crdb-sql <name>`) and an
MCP tool (`<name>`). The table below reflects what the current build
registers in [`internal/mcp/server.go`](internal/mcp/server.go) and
[`cmd/root.go`](cmd/root.go).

| MCP tool / CLI command | Tier | Purpose |
|------------------------|------|---------|
| `validate_sql` / `validate` | 1-2 | Parse + type-check + (with `--schema`) name-resolve. Returns structured errors with positions, available names, and "did you mean?" suggestions. |
| `format_sql` / `format` | 1 | Canonicalize SQL with optional syntax highlighting. Auto-strips `cockroach sql` shell prompts from pasted input. |
| `parse_sql` / `parse` | 1 | Statement classification (DDL/DML/DCL/TCL), tag, fingerprint, parser version. |
| `detect_risky_query` / `risk` | 1 | AST-only risk assessment with reason codes and fix hints (DELETE without WHERE, missing-WHERE UPDATE, DDL hygiene, etc.). |
| `summarize_sql` / `summarize` | 1 | Structured per-statement summary (tables touched, operations). |
| `list_tables` / `list-tables` | 2-3 | Tables from loaded `--schema` files or a live `--dsn` cluster. |
| `describe_table` / `describe` | 2-3 | Columns, types, nullability, primary key, indexes. |
| `explain_sql` / `explain` | 3 | EXPLAIN output as structured JSON. `--mode read_only` (default) admits the read-only set; `--mode safe_write` additionally admits DML so the planner returns a plan for inner writes; `--mode full_access` admits anything that parses (the cluster-side read-only txn wrapper still surfaces SQLSTATE 25006 for inner DDL). Requires `--dsn`. |
| `explain_schema_change` / `explain-ddl` | 3 | EXPLAIN (DDL, SHAPE) — schema-change plan with phases and operations. The default `read_only` mode rejects every DDL (since DDL modifies schema), so this command requires `--mode safe_write` (admits DDL but rejects DCL/cluster-admin/non-DDL) or `--mode full_access` (admits any DDL that parses). Requires `--dsn`. |
| `simulate_sql` / `simulate` | 3 | Side-effect-free by construction: dispatches each statement to a non-mutating EXPLAIN flavor — SELECT runs through `EXPLAIN ANALYZE` inside `BEGIN READ ONLY` (the read does execute, but writes are blocked at the cluster), DML through plain `EXPLAIN` (planner-only, write never applied), DDL through `EXPLAIN (DDL, SHAPE)` (planner-only). Requires `--dsn`. |
| `execute_sql` / `exec` | 3 | Run SQL against the cluster behind the safety allowlist. `--mode read_only` (default) admits the same set as `explain`; `--mode safe_write` additionally admits DML; `--mode full_access` admits anything that parses. Schema/privilege/cluster-admin ops require `full_access`. Requires `--dsn`. |

**Three-tier progressive capability:**

- **Tier 1 — Zero-config:** parse, format, classify, type-check expressions.
  Works offline with no setup.
- **Tier 2 — Schema files:** load `CREATE TABLE` files for name resolution,
  column type validation, and "did you mean?" suggestions. No cluster
  needed.
- **Tier 3 — Connected:** EXPLAIN, schema-change analysis, and live
  catalog introspection against a CockroachDB cluster via `--dsn` (or
  `CRDB_DSN`).

## Installation

### Prerequisites

- Go 1.26+ (Go's `toolchain` directive will auto-download the matching
  toolchain on first build if your local install is older but compatible).

### Option A — Build from source (canonical)

```bash
git clone https://github.com/spilchen/sql-ai-tools.git
cd sql-ai-tools
make build
```

Produces `bin/crdb-sql`. Add `bin/` to your `PATH` or copy the binary
somewhere on it.

### Option B — `go install`

```bash
go install github.com/spilchen/sql-ai-tools/cmd/crdb-sql@latest
```

This installs the unsuffixed `crdb-sql` (latest quarter) into
`$GOBIN`. It does **not** install the per-quarter `crdb-sql-vXXX`
siblings shipped in the release archives — those are needed when you
pass `--target-version` for an older quarter. Without the matching
sibling on `$PATH`, `crdb-sql` prints a discovery hint to stderr and
exits with status 2 rather than silently falling back to the wrong
parser. Install the matching `crdb-sql-vXXX` from a release archive
when you need an older quarter.

### Verify

```bash
crdb-sql version
# crdb-sql: dev
# cockroachdb-parser: v0.26.2
# builtins-stubs: v26.2
```

## Quickstart

The same scenario from
[`docs/hackathon_plan.md`](docs/hackathon_plan.md) §Demo Vision, as three
copy/pasteable blocks — one per tier.

### Tier 1 — Zero-config (no cluster, no schema)

Catch a type mismatch before it ever reaches a cluster:

```bash
crdb-sql validate -o json -e "SELECT 1 + 'hello'"
```

```json
{
  "tier": "zero_config",
  "parser_version": "v0.26.2",
  "connection_status": "disconnected",
  "errors": [
    {
      "code": "capability_required",
      "severity": "WARNING",
      "message": "name resolution skipped: --schema not provided",
      "category": "capability_required",
      "context": {
        "capability": "name_resolution",
        "hint": "pass --schema FILE to enable table name resolution"
      }
    },
    {
      "code": "22023",
      "severity": "ERROR",
      "message": "unsupported binary operator: <int> + <string>",
      "position": {"line": 1, "column": 8, "byte_offset": 7}
    }
  ],
  "data": {
    "valid": false,
    "checks": {
      "syntax": "ok",
      "function_resolution": "ok",
      "type_check": "failed",
      "name_resolution": "skipped"
    }
  }
}
```

Without `--schema`, the `capability_required` WARNING tells the agent
that name resolution did not run — even when the rest of validation
passes — so a partial-coverage result is never silently mistaken for a
clean one.

### Tier 2 — Schema-aware (no cluster, schema files only)

Drop a `CREATE TABLE` file alongside your queries and get
"did you mean?" suggestions on misspelled column names:

```bash
cat > schema.sql <<'EOF'
CREATE TABLE users (
  id INT PRIMARY KEY,
  name STRING,
  email STRING
);
EOF

crdb-sql validate -o json --schema schema.sql -e "SELECT nme FROM users"
```

```json
{
  "tier": "schema_file",
  "parser_version": "v0.26.2",
  "connection_status": "disconnected",
  "errors": [
    {
      "code": "42703",
      "severity": "ERROR",
      "message": "column \"nme\" does not exist",
      "position": {"line": 1, "column": 8, "byte_offset": 7},
      "category": "unknown_column",
      "context": {"available_columns": ["id", "name", "email"]},
      "suggestions": [
        {
          "replacement": "name",
          "range": {"start": 7, "end": 10},
          "confidence": 0.75,
          "reason": "levenshtein_distance_1"
        }
      ]
    }
  ],
  "data": {
    "valid": false,
    "checks": {
      "syntax": "ok",
      "function_resolution": "ok",
      "type_check": "ok",
      "name_resolution": "failed"
    }
  }
}
```

The agent reads the structured `suggestions[].replacement`, applies the
fix, and re-runs — no second LLM round-trip needed.

### Tier 3 — Connected (live cluster)

Start a single-node cluster (either is fine):

```bash
# If you have the cockroach binary installed:
cockroach demo --no-example-database

# Or via Docker (cleanup: docker rm -f crdb when done):
docker run -d --name crdb -p 26257:26257 \
  cockroachdb/cockroach:latest start-single-node --insecure
```

Point `crdb-sql` at it via `--dsn` (or set `CRDB_DSN` once), then load
the same `users` schema from Tier 2 into the cluster so the
introspection tools have something to inspect:

```bash
export CRDB_DSN="postgresql://root@localhost:26257/defaultdb?sslmode=disable"

# Load schema.sql (created in the Tier 2 example) into the cluster:
psql "$CRDB_DSN" -f schema.sql
# or, if psql isn't installed: cockroach sql --insecure -f schema.sql

crdb-sql ping -o json
crdb-sql list-tables -o json
crdb-sql describe users -o json
```

Example `describe` output against a `users` table:

```json
{
  "tier": "connected",
  "parser_version": "v0.26.2",
  "connection_status": "connected",
  "data": {
    "name": "users",
    "columns": [
      {"name": "id",    "type": "INT8",   "nullable": false},
      {"name": "name",  "type": "STRING", "nullable": true},
      {"name": "email", "type": "STRING", "nullable": true}
    ],
    "primary_key": ["id"],
    "indexes": []
  }
}
```

`crdb-sql explain --dsn ... -e "SELECT ..."` returns the EXPLAIN plan
as structured JSON, and `crdb-sql simulate --dsn ... -e "..."` runs a
side-effect-free dispatch (see the catalog row for the exact rules).
For actual writes, `crdb-sql exec --dsn ... --mode safe_write -e
"..."` runs SQL behind the safety allowlist; the default
`--mode read_only` admits the same shape as `explain`. `crdb-sql
explain-ddl --dsn ... --mode safe_write -e "ALTER TABLE ..."`
returns the declarative schema-change plan — read_only rejects DDL
by design, so this command requires safe_write or full_access.

### Connecting to a secure cluster

`crdb-sql` talks to TLS-only clusters with no extra setup: pgx accepts
the standard libpq URI parameters (`sslmode`, `sslrootcert`, `sslcert`,
`sslkey`) inside the DSN, and the same four are also exposed as
top-level `--ssl*` flags for the CLI and as input fields on every
connected MCP tool.

Password-based auth with a CA cert (typical for a managed cluster):

```bash
crdb-sql ping --dsn "postgresql://root@host:26257/defaultdb?sslmode=verify-full&sslrootcert=/path/ca.crt"
```

Client-certificate auth (typical for a self-managed cluster's `root`
user):

```bash
crdb-sql ping --dsn "postgresql://root@host:26257/defaultdb?sslmode=verify-full&sslrootcert=/path/ca.crt&sslcert=/path/client.root.crt&sslkey=/path/client.root.key"
```

Equivalent invocation using the per-knob flags (the flag values are
merged into the DSN before connect, and the merge fails loudly if the
same parameter is supplied on both sides):

```bash
crdb-sql ping \
  --dsn "postgresql://root@host:26257/defaultdb" \
  --sslmode verify-full \
  --sslrootcert /path/ca.crt \
  --sslcert /path/client.root.crt \
  --sslkey /path/client.root.key
```

The same fields appear on the MCP tool input schemas, so an agent
configuring `explain_sql` (or any other connected tool) can supply
TLS knobs without knowing the libpq URI form:

```json
{
  "sql": "SELECT 1",
  "dsn": "postgresql://root@host:26257/defaultdb",
  "sslmode": "verify-full",
  "sslrootcert": "/path/ca.crt"
}
```

## Use as an MCP server

`crdb-sql mcp` runs the binary as a Model Context Protocol server over
stdio. Every tool in the catalog above is registered.

Register the binary with Claude Code. After Option A (build from
source), point at the build output directly:

```bash
claude mcp add crdb-sql -- "$(pwd)/bin/crdb-sql" mcp
```

After Option B (`go install`), use the bare command name (the
installed binary is on `$PATH` via `$GOBIN`):

```bash
claude mcp add crdb-sql -- crdb-sql mcp
```

The leading `--` is required so the `mcp` argument is forwarded to
`crdb-sql` instead of being parsed by the `claude` CLI. No transport
flags are needed — `claude mcp add` defaults to stdio, which is what
this server speaks.

Verify discovery from inside Claude Code:

```
/mcp
```

`crdb-sql` should appear with all registered tools. Calling the
health-check tool returns:

```json
{"ok": true, "parser_version": "v0.26.2"}
```

The `parser_version` value should match the `cockroachdb-parser:` line
from `crdb-sql version`.

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

## Development

```bash
make test    # go test ./...
make lint    # gofmt check + go vet + golangci-lint (CI gate)
make fmt     # auto-format sources
```

`make lint` is the CI gate. `go fmt` violations do not block `make build`,
so configure your editor to run `gofmt`/`goimports` on save.

## Project status

Tier 1 and Tier 2 are usable today. Tier 3 connected tools (`ping`,
`list-tables`, `describe`, `explain`, `explain-ddl`, `simulate`,
`exec`) work; `exec`, `explain`, and `explain-ddl` honour all three
safety modes (`read_only`, `safe_write`, `full_access`). `simulate`
is the remaining Tier 3 surface whose `safe_write`/`full_access`
wiring is still follow-up work.

See [`docs/`](docs/) for the design document, hackathon plan, and
research lessons that inform the architecture.

## License

Apache License 2.0. See [LICENSE](LICENSE) for details.
