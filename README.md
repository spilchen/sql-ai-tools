# sql-ai-tools

Agent-friendly SQL tooling for CockroachDB. An MCP server and CLI that wraps
CockroachDB's parser, type system, and error infrastructure in a structured,
agent-consumable interface.

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

- Go 1.23+

### Build

```bash
go build -o sql-ai-tools .
```

### Run

```bash
# Parse and pretty-print a SQL statement
./sql-ai-tools
# Output: SELECT 1
```

## Project Status

Early development. See [docs/](docs/) for the design document, hackathon plan,
and research lessons that inform the architecture.

## License

TBD
