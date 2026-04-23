# Eng Design Doc: Agent-Friendly SQL Tooling for CockroachDB

## Approval

| Reviewer | Team | Role (DACI Matrix) | Approved? |
|----------|------|--------------------|-----------|
| Matt Spilchen | SQL Foundations | Driver | |
| | SQL Execution | Approver | |
| | Developer Experience | Approver | |
| | SQL Schema | Informed | |

## Motivation

AI agents write SQL for CockroachDB constantly — and get it wrong in subtle
ways. The current agent workflow is: generate SQL, send it to a running
cluster, parse the English error string, guess at a fix, retry. This loop
costs 3-5 LLM calls per error, burns compute and latency, and produces
fragile agents that break on unfamiliar schema.

CockroachDB has a world-class SQL parser, type system, built-in function
registry, and structured error infrastructure — but these capabilities are
trapped inside the server binary. No standalone tool exposes CRDB's SQL
intelligence as structured, agent-consumable operations.

The opportunity is to build that tool: an MCP server and CLI that wraps
CRDB's existing parser primitives in a structured, agent-friendly interface.
The tool gives agents the ability to validate, explain, and safely execute SQL
with the exact fidelity of the CockroachDB server — offline, from schema files,
or connected to a live cluster.

**Why now:**

- The `cockroachdb-parser` standalone Go module (v0.25.2) makes parser
  extraction a solved problem. The tool can be built on top of an existing,
  maintained module — no extraction work needed.
- The MCP specification is maturing as the standard for AI agent tool
  integration. Claude Code, VS Code Copilot, and other agent platforms all
  support MCP.
- CRDB already has powerful capabilities (EXPLAIN (DDL) for schema change
  impact, `sql_safe_updates` for statement safety, 8 EXPLAIN modes) that are
  undiscoverable to agents because they are buried in SQL syntax. The tool
  surfaces these as first-class named operations.

**The agent loop today vs. with this tool:**

```
Today:     write SQL → execute → parse error string → guess fix → retry (×5)
With tool: write SQL → validate_sql → read JSON → apply fix → execute (×1)
```

Agents need a tight loop: write → validate → fix → execute. CRDB today enters
that loop at "execute," which is too late. This tool enters at "validate,"
which is where the leverage is.

**Cross-vendor analysis**: A systematic study of four SQL tooling ecosystems
(syntaqlite, pg_query, sqlc, and CockroachDB) across 25 research dimensions
confirms a consistent architectural pattern: real parser extraction + schema
loading + semantic analysis + structured output = agent-friendly SQL tooling.
Every external tool validates that using the real database parser (not
approximating the grammar) produces dramatically better tooling. The consistent
gap across all tools is agent-friendly error output — errors are designed for
humans, not programmatic consumers. This is the primary opportunity.

## Noteworthy Requirements

### Product

- **Standalone SQL validation without a running cluster.** The core value
  proposition: parse, type-check, and validate SQL using the real CRDB parser
  offline. This catches syntax errors, type mismatches, unknown columns, and
  misspelled identifiers before any network round-trip.

- **Schema-aware analysis from files.** Load CREATE TABLE statements from SQL
  files to enable name resolution, column type validation, and "did you mean?"
  suggestions — all without a live database connection. Schema files are the
  source of truth for the offline tier.

- **Structured error output for AI agents.** Every error includes SQLSTATE
  code, severity, line/column position, error category, available alternatives
  (for unknown-name errors), expected types (for type mismatches), and
  structured fix suggestions with replacement text and byte ranges. The output
  format is JSON, not error strings.

- **MCP server for native agent integration.** The primary delivery surface is
  an MCP server over stdio, discoverable by Claude Code and other MCP-compatible
  agent platforms. Tools are named to teach a safe workflow.

- **CLI for CI/CD and shell-out patterns.** A CLI mirrors the MCP tools for
  agents that use shell-out instead of MCP, and for CI/CD pipeline integration.
  Supports `--output json` and `--output text` modes.

- **Zero-config value from built-in knowledge.** The tool provides immediate
  value with zero setup: parse, format, classify, type-check expressions, and
  detect risky patterns. Built-in function signatures (from populated builtins
  stubs) enable function name validation and overload resolution offline.

### User Interfaces

**MCP tools** (via stdio transport):

The tool palette is ordered to teach a safe workflow: validate → explain →
simulate → execute.

| Tool | Input | Output | Tier |
|------|-------|--------|------|
| `validate_sql` | SQL string, optional schema files | Structured error list with positions, suggestions, available names | 1-2 |
| `format_sql` | SQL string, optional `--color` | Canonicalized SQL, optionally with ANSI syntax highlighting. Auto-strips `cockroach sql` shell prompts (`root@...>`, `->`) from pasted input. | 1 |
| `parse_sql` | SQL string | Statement type (DDL/DML/DCL/TCL), tag, fingerprint, parser version | 1 |
| `list_tables` | — | Table names from loaded schema or live catalog | 2-3 |
| `describe_table` | Table name | Columns, types, constraints, indexes | 2-3 |
| `explain_sql` | SQL string | EXPLAIN output as structured JSON | 3 |
| `explain_schema_change` | DDL string | Schema changer plan with phases, elements, operations | 3 |
| `detect_risky_query` | SQL string | Risk assessment with reason codes, severity, fix hints | 1-3 |
| `simulate_sql` | SQL string | Per-statement EXPLAIN-based simulation (ANALYZE for SELECT, plain EXPLAIN for DML writes, EXPLAIN (DDL, SHAPE) + table stats for DDL); no inner statement is executed at the cluster level | 3 |
| `execute_sql` | SQL string | Query results with safety guardrails | 3 |

**CLI** (mirrors MCP tools):

```bash
crdb-sql validate [--schema FILE] [--output json|text] [-e SQL | FILE | stdin]
crdb-sql format [--color] [-e SQL | FILE | stdin]  # auto-strips cockroach sql shell prompts
crdb-sql parse [-e SQL | FILE | stdin]
crdb-sql explain [--dsn DSN] [-e SQL]
crdb-sql explain-ddl [--dsn DSN] [-e DDL]
crdb-sql check [--dsn DSN] [--schema FILE] [-e SQL | FILE]
```

**Error output** (JSON):

```json
{
  "errors": [
    {
      "code": "42703",
      "severity": "ERROR",
      "message": "column \"nme\" does not exist",
      "position": {"line": 1, "column": 8, "byte_offset": 7},
      "category": "unknown_column",
      "context": {
        "table": "users",
        "available_columns": ["id", "name", "email", "created_at"]
      },
      "suggestions": [
        {
          "replacement": "name",
          "range": {"start": 7, "end": 10},
          "confidence": 0.9,
          "reason": "levenshtein_distance_1"
        }
      ]
    }
  ],
  "statement_type": "SELECT",
  "tier": "schema_file",
  "parser_version": "v25.2.5",
  "connection_status": "disconnected"
}
```

### Non-functional Requirements

- **Parser fidelity.** The tool must parse SQL identically to CockroachDB.
  This is the value proposition — a generic SQL parser that misses CRDB-specific
  syntax (hash-sharded indexes, regional tables, PL/pgSQL extensions) is worse
  than no parser. Achieved by importing `cockroachdb-parser`, which IS the CRDB
  parser.

- **Offline operation.** Core tools (parse, format, validate, type-check) must
  work without any cluster connection or network access. The tool must be useful
  on an airplane.

- **Lightweight binary.** The tool must not require the full cockroach binary
  (~300-500 MB). Target: 10-30 MB standalone binary. Precedent: crlfmt (4.2 MB),
  optfmt (5.3 MB). The `cockroachdb-parser` module has 59 transitive deps and
  9.6 MB module cache — binary size is achievable.

- **Safe by default when connected.** When connected to a live cluster, the
  default mode is read-only. Statement allowlists enforce SELECT/SHOW/EXPLAIN
  only. LIMIT injection prevents unbounded queries. Statement timeouts prevent
  runaway operations. Defense-in-depth: MCP-layer filtering + restricted DB
  user + optional proxy.

- **Progressive capability unlock.** The tool must be useful at zero setup,
  more useful with schema files, and most useful with a connection. All tools
  are always visible — tools requiring a higher tier return structured errors
  explaining what setup is needed, not silent failures or hidden tools.

- **Connection credential safety.** Connection credentials must never be stored
  in plain config files. Credentials come from environment variables
  (`CRDB_DSN`) or OS keychain integration. Config files specify non-secret
  connection parameters only (endpoint, database, sslmode).

## Dependency Relationships

- **cockroachdb-parser module**: Load-bearing dependency. Maintained by a
  single internal contributor with infrequent updates (~1 snapshot per CRDB
  major release). Planned approach: fork the repo, update to CRDB 26.2, and
  add builtin stubs directly. Fork maintenance is ~half day per CRDB release.
  Upstream contribution post-hackathon.

- **SQL Execution / SQL Schema teams**: The tool wraps existing CRDB
  capabilities (parser, EXPLAIN, EXPLAIN (DDL), pgerror) without modifying
  them. No cross-team blocking dependencies for the initial version. Future
  enhancements (VALIDATE QUERY syntax, EXPLAIN ANALYZE NO_WRITE mode,
  EXPLAIN (DDL) impact estimates) would require collaboration.

- **Developer Experience team**: The MCP server and CLI are new developer
  tooling surfaces. Coordination on naming, distribution (Homebrew, goreleaser),
  and documentation is needed before GA.

- **MCP SDK**: Using mark3labs/mcp-go for initial development. Tool interface
  is portable to the official modelcontextprotocol/go-sdk. The SDK choice is
  not a lock-in risk.

## Detailed Implementation

### Parser Extraction / Wrapping Strategy

The parser extraction question is settled: import `cockroachdb-parser` v0.25.2
as a Go module. The module provides:

| Component | Package | What It Provides |
|-----------|---------|-----------------|
| Parser | `pkg/sql/parser` | Parse(), ParseOne(), ParseExpr(), ParseTableName(), Tokens() |
| AST | `pkg/sql/sem/tree` | Statement interface, Visitor/ExtendedVisitor, 30+ FmtFlags, PrettyCfg |
| Builtins registry | `builtinsregistry` | Register(), GetBuiltinProperties(), AddSubscription() — mechanism works, registry EMPTY |
| Type system | `pkg/sql/types` | All SQL types, OID mapping, casting rules — fully self-contained, no sem/tree dependency |
| Error handling | `pgerror` | SQLSTATE codes, severity, hint, detail, Flatten() API |
| Help system | `pkg/sql/parser/help` | HelpMessages map with per-statement syntax documentation |

**The extraction boundary**: The parser, AST, types, and pgerror are
extractable (94 internal deps, no KV/storage). The optbuilder (name resolution,
full type checking) is NOT extractable due to deep optimizer framework
dependencies. The tool builds new simplified semantic analysis above the
parser extraction, not a stripped-down optbuilder.

**Builtins registry**: The standalone extraction registers ZERO built-in
functions — all 26 definition files are excluded by `snapshot.sh` because
metadata and implementation closures are interleaved (`builtins.go` alone
imports 77 CRDB packages). This is a BLOCKING dependency for function name
validation, type checking, return type inference, and autocomplete. The fix:
auto-generate metadata-only stubs (~876+ entries) containing name, argument
types, return type, volatility, and class — no Fn closures. `Register()` is
exported, so stubs can live in consumer code.

**Version strategy**: Import the published module version. Version-specific
features are edge cases. If parser modifications are needed (e.g., exposing
`sqlErrorMessage` internals for autocomplete), the fork-and-patch approach
provides a clean upgrade path.

### Schema Loader Architecture

The schema loader parses CREATE TABLE files using the CRDB parser itself and
builds a lightweight in-memory catalog:

```
SQL DDL files → CRDB parser → tree.CreateTable AST → Lightweight Catalog
                                                      ├── Tables[]
                                                      │   ├── Name
                                                      │   ├── Columns[]
                                                      │   │   ├── Name
                                                      │   │   ├── Type (via SQLString())
                                                      │   │   ├── Nullable
                                                      │   │   └── Default
                                                      │   ├── PrimaryKey
                                                      │   └── Indexes[]
                                                      └── Name index (for fuzzy matching)
```

**Design decisions:**

- **Dogfooding the parser for schema loading** ensures exact syntax
  compatibility with all CRDB DDL including extensions (USING HASH, regional
  tables, computed columns). No secondary parser needed.

- **ResolvableTypeReference.SQLString()** provides type information without
  importing `pkg/sql/types` directly — keeping the schema loader lightweight.

- **Levenshtein-distance suggestions** for misspelled table and column names.
  When a name is not found in the catalog, the loader computes edit distances
  against all known names and suggests the closest match above a confidence
  threshold.

**Known edge cases** (from prototype documentation):
- `PrimaryKey` on `ColumnTableDef` does not set `Nullability` to `NotNull` —
  must handle explicitly
- Inline UNIQUE constraints on columns don't get names from the parser — must
  synthesize index names
- ALTER TABLE, CREATE INDEX, CREATE TYPE need additional parsing support beyond
  the initial CREATE TABLE loader

**Configuration model**: Schema files are specified via CLI flags
(`--schema schema.sql`) for simple use, or via a YAML config file with
glob-based schema-to-query file mapping for projects:

```yaml
version: 1
sql:
  - schema: ["schema/*.sql"]
    queries: ["queries/**/*.sql"]
  - schema: ["test/schema.sql"]
    queries: ["test/**/*.sql"]
```

### Semantic Checker Design

The semantic checker is a new, simplified analysis layer above the parser. It
does NOT reuse or extract the optbuilder — it implements a subset of semantic
analysis against the lightweight catalog.

**Three analysis tiers:**

**Tier 1 — Expression type checking (zero-config):**
- Use `MakeSemaContext(nil)` for standalone expression type checking
- Binary operator type mismatch detection (`1 + 'hello'` → error)
- CAST with built-in types (works without TypeResolver)
- COALESCE, CASE, NULLIF type resolution
- Wrap GREATEST/LEAST in `recover()` for graceful degradation (1/18 panics)
- Function call validation against populated builtins registry

**Tier 2 — Name resolution (requires schema files):**
- Table existence checking against loaded catalog
- Column existence checking against table schema
- "Did you mean?" suggestions via Levenshtein distance
- Column count validation in INSERT/UPDATE
- Column type validation in INSERT/UPDATE (expression type vs column type)

**Tier 3 — Connected validation:**
- Delegate to CRDB via EXPLAIN for plan-level validation
- Live schema introspection from `information_schema`
- Privilege checking

**Multi-error accumulation**: The semantic checker continues after errors and
accumulates all diagnostics into a single response. After a name resolution
failure for a table, subsequent column references against that table are
suppressed (not cascaded as false errors). This is an architectural decision
made at design time: each scope entry tracks whether it resolved successfully,
and downstream checks skip entries in error state.

### Error Enrichment

The error enrichment layer wraps pgerror output with agent-specific context:

1. **Position computation**: Compute line/column from byte offset in the
   Detail field's caret marker. The code in `PopulateErrorDetails` already
   computes line boundaries internally — extracting numeric line/column is
   straightforward string processing.

2. **Error categorization**: Map SQLSTATE codes to higher-level categories
   (`unknown_column`, `type_mismatch`, `syntax_error`, `unknown_function`,
   `ambiguous_reference`) that agents can switch on without parsing message
   text.

3. **Context enrichment**: For unknown-name errors, include the list of
   available names from the relevant scope (columns for unknown column, tables
   for unknown table, functions for unknown function). For type mismatches,
   include expected and actual types.

4. **Suggestion generation**: Levenshtein-distance matching against available
   names. Each suggestion includes replacement text, byte range in the original
   SQL, and confidence score. Suggestions are structured objects, not hint
   strings.

5. **Tier annotation**: Every response includes the tier that produced it
   (`zero_config`, `schema_file`, `connected`) so agents know what level of
   validation was performed.

### MCP Server Architecture

```
┌──────────────────────────────────────┐
│           MCP Client (Agent)         │
│  (Claude Code, VS Code, etc.)       │
└──────────┬───────────────────────────┘
           │ stdio (JSON-RPC)
┌──────────▼───────────────────────────┐
│           MCP Server                 │
│  ┌─────────────────────────────────┐ │
│  │    Tool Router                  │ │
│  │    (validate, format, explain)  │ │
│  └──────────┬──────────────────────┘ │
│             │                        │
│  ┌──────────▼──────────────────────┐ │
│  │    Validation Engine            │ │
│  │    ├── Parser (cockroachdb-parser)│
│  │    ├── Type Checker (SemaContext)│ │
│  │    ├── Name Resolver (catalog)  │ │
│  │    ├── Error Enricher           │ │
│  │    └── Risk Detector (rules)    │ │
│  └──────────┬──────────────────────┘ │
│             │                        │
│  ┌──────────▼──────────────────────┐ │
│  │    Schema Catalog               │ │
│  │    ├── File Loader              │ │
│  │    └── Live Introspector        │ │
│  └──────────┬──────────────────────┘ │
│             │                        │
│  ┌──────────▼──────────────────────┐ │
│  │    Connection Manager           │ │
│  │    ├── pgwire (pgx)             │ │
│  │    ├── Safety Layer             │ │
│  │    └── EXPLAIN Wrapper          │ │
│  └─────────────────────────────────┘ │
└──────────────────────────────────────┘
```

**Transport**: stdio for local MCP (JSON-RPC over stdin/stdout). No HTTP server
— it adds attack surface without benefit for a local tool. For cluster-connected
features, the server connects to CockroachDB via pgwire with TLS as a client.

**SDK**: mark3labs/mcp-go for initial development. The tool interface (tool
names, schemas, handlers) is portable between Go MCP SDKs. Migration to the
official modelcontextprotocol/go-sdk is a mechanical refactor when the official
SDK matures.

**Tool registration**: Each tool is registered with a name, JSON schema for
inputs, and a handler function. The handler returns structured JSON.
Tools that require a higher capability tier return structured
`capability_required` errors rather than failing silently.

### Safety Model

**Three enforced modes** (not advisory — enforced by construction):

| Mode | Default | Allowed Statements | Guardrails |
|------|---------|-------------------|------------|
| `read_only` | Yes | SELECT, SHOW, EXPLAIN, SET (session only) | LIMIT injection, statement timeouts, read-only txn |
| `safe_write` | No | Above + INSERT, UPDATE, DELETE | `sql_safe_updates` enabled, WHERE required for UPDATE/DELETE, row caps, confirmation hooks |
| `full_access` | No | All | Warning on dangerous statements, audit log |

**Defense-in-depth** (three independent layers):

1. **MCP-layer**: Statement allowlist parsed from the SQL AST before execution.
   `StatementType()`, `CanModifySchema()`, `CanWriteData()` provide
   classification primitives. Denied statements return structured errors with
   the required safety mode.

2. **DB-user**: The recommended setup uses a restricted database user with
   only SELECT, SHOW, and EXPLAIN privileges. Even if the MCP layer has a bug,
   the database rejects unauthorized operations.

3. **Optional proxy**: A SQL proxy (e.g., pgbouncer with query restrictions)
   provides a third layer of defense for high-security environments.

**EXPLAIN ANALYZE policy**: EXPLAIN ANALYZE executes the query to collect
runtime statistics. In `read_only` mode, EXPLAIN ANALYZE is allowed only for
read-only statements (SELECT without side-effecting functions). For
write statements, only EXPLAIN (no ANALYZE) is permitted.

### Progressive Capability Architecture

Three tiers in one binary:

```
Tier 1: Zero-Config (always available)
├── Parse any CockroachDB SQL
├── Format/pretty-print (FmtParsable, PrettyCfg)
├── Classify statements (DDL/DML/DCL/TCL)
├── Type-check expressions (MakeSemaContext(nil))
├── Detect risky patterns (AST-only rules)
├── Fingerprint queries (FmtHideConstants)
├── Statement help (HelpMessages)
└── Function validation (with populated stubs)

Tier 2: Schema-File (requires --schema or config)
├── Name resolution (table/column existence)
├── Column type validation (INSERT/UPDATE)
├── "Did you mean?" suggestions (Levenshtein)
├── Schema-aware risk detection (large tables)
├── list_tables / describe_table
└── Multi-error reporting

Tier 3: Connected (requires --dsn or CRDB_DSN)
├── EXPLAIN with real statistics
├── EXPLAIN (DDL) with SHAPE mode
├── Live schema introspection
├── Privilege validation
├── simulate_sql (txn+rollback)
└── execute_sql (with guardrails)
```

**Visibility principle**: All tools are always shown in the MCP tool catalog.
Tools requiring a higher tier return structured errors:

```json
{
  "error": "schema_required",
  "message": "validate_sql can check syntax without schema, but name resolution requires schema files",
  "hint": "Pass schema files via --schema flag or create a crdb-sql.yaml config",
  "partial_result": {
    "syntax": "valid",
    "type_check": "valid",
    "name_resolution": "skipped"
  }
}
```

This is more agent-friendly than hiding tools: agents discover what exists
and learn what setup unlocks more capability.

### Rule-Based Analysis Engine

`detect_risky_query` and `summarize_sql` are deterministic, rule-based tools
over AST, catalog metadata, and plan output. No LLM needed.

**Rule registry architecture:**

```go
type Rule struct {
    ID          string          // "delete_without_where"
    Level       RuleLevel       // ASTOnly, CatalogAware, PlanAware
    Severity    Severity        // Critical, High, Medium, Low, Info
    Category    string          // "data_safety", "performance", "schema_change"
    Check       func(ctx) []Finding
    Message     string          // Template: "DELETE on {{.Table}} without WHERE clause"
    FixHint     string          // "Add a WHERE clause to limit affected rows"
}

type Finding struct {
    RuleID      string
    Severity    Severity
    Message     string          // Rendered message
    Position    Position        // Line/column in SQL
    ReasonCode  string          // Machine-readable: "DELETE_NO_WHERE"
    FixHint     string
    Context     map[string]any  // Rule-specific context
}
```

**Statement summarization** (`summarize_sql`):

```json
{
  "operation": "DELETE",
  "tables": ["orders"],
  "predicates": ["status = 'cancelled'", "created_at < '2024-01-01'"],
  "affected_columns": [],
  "joins": [],
  "risk_level": "medium",
  "summary": "Delete rows from orders where status is cancelled and created before 2024-01-01"
}
```

### Connection Management

- **Config (non-secret)**: Endpoint, database, sslmode, application_name
  specified in `crdb-sql.yaml` config file or CLI flags.
- **Secrets**: Connection string with credentials via `CRDB_DSN` environment
  variable. Never stored in config files. OS keychain integration as a future
  enhancement.
- **Connection lifecycle**: Lazy connection — the server connects only when a
  Tier 3 tool is first called. Connection status is always reported in tool
  responses via a `connection_status` field.
- **Reconnection**: Automatic reconnection with exponential backoff on
  transient failures. Structured error on persistent connection failure.

### Testing Strategy

**Unit tests**: Each component (parser wrapper, schema loader, name resolver,
type checker, error enricher, risk detector) is tested independently with
table-driven tests. Go's testing package with subtests.

**Integration tests**: End-to-end tests through the MCP tool handlers. Input:
SQL string + optional schema files. Output: structured JSON response. These
tests verify the full pipeline from tool invocation to structured output.

**Fidelity tests**: Parse a corpus of known-valid CRDB SQL (from CRDB's own
test suite) to verify the standalone parser matches the in-server parser.
Any parse disagreement is a bug.

**Safety tests**: Attempt to execute mutating SQL in `read_only` mode. Verify
that the statement allowlist correctly rejects all write operations. Test
defense-in-depth by verifying that the DB user also rejects writes.

**Agent integration tests**: Use Claude Code with the MCP server to validate
real SQL generation workflows. Verify that structured error output enables
single-iteration error correction.

## Alternatives Considered

### Approximate parser (rejected)

Using a generic SQL parser (e.g., sqlparser-rs, ANTLR grammar) instead of
the real CRDB parser. **Rejected because fidelity is the entire value
proposition.** A parser that accepts SQL that CRDB rejects (or vice versa)
produces false confidence or false errors — both worse than no parser. The
cross-vendor analysis confirms this universally: syntaqlite achieves 99.7%
fidelity by using SQLite's real Lemon grammar; pg_query extracts PostgreSQL's
real parser. Approximation is never worth it.

### Live cluster connection for schema (rejected as sole method)

Requiring a live cluster connection for all schema-aware validation.
**Rejected because files are better for agents.** Agents operate in
environments where a cluster may not be available (CI/CD, local development,
air-gapped environments). Schema files provide deterministic, reproducible
validation. The live connection is additive (Tier 3), not required.

### Piggyback on cockroach binary (rejected)

Running the full `cockroach` binary in a subprocess and parsing its output.
**Rejected because it is too heavy for agent workflows.** The cockroach binary
is 300-500 MB, has complex startup requirements, and produces human-oriented
output. A lightweight standalone binary (10-30 MB) with structured JSON output
is the correct delivery for both MCP and CLI.

### Generic SQL linting (rejected)

Building on an existing SQL linter (sqlfluff, squawk, etc.) instead of CRDB
internals. **Rejected because CRDB-specific is the value.** Generic linters
cannot validate CRDB-specific syntax (hash-sharded indexes, USING HASH,
regional tables), cannot check against CRDB's type system and casting rules,
and cannot surface CRDB-specific capabilities like EXPLAIN (DDL). The tool
must understand CRDB as well as CRDB understands itself.

### Extract the optbuilder (rejected)

Extracting CRDB's optbuilder (`pkg/sql/opt/optbuilder`) for standalone name
resolution and type checking. **Rejected because the optbuilder is not
extractable.** It has deep dependencies on the optimizer framework (opt, memo,
norm, props), cluster version, privileges, and the full catalog interface. The
import graph makes standalone extraction infeasible without a major refactoring
effort. Building a simplified semantic checker above the parser extraction is
the pragmatic alternative.

## Future Work

### Simulation Layer

`simulate_sql` (issue #28, shipped) takes a per-statement
EXPLAIN-based dispatcher rather than the txn+rollback wrapper that
issue #28 originally proposed. The dispatcher routes each parsed
statement to a non-executing EXPLAIN flavor:

- SELECT (and other read-only DML) → `EXPLAIN ANALYZE` inside a
  read-only txn. ANALYZE physically executes the inner statement, but
  SELECT has no side effects, so the runtime stats (rows read,
  network bytes, time) come back without persisting anything.
- INSERT/UPDATE/DELETE/UPSERT → plain `EXPLAIN`. Returns the
  optimizer's estimated plan only — the write is never applied. We
  trade measured stats for honest safety here.
- DDL → `EXPLAIN (DDL, SHAPE)` plus `SHOW STATISTICS` for each
  affected table. The declarative schema changer compiles a plan
  without applying it; the row-count annotation gives the agent a
  rough handle on backfill cost.

This sidesteps the side-effect leaks the txn+rollback approach
cannot prevent (sequence increments, volatile function side effects,
audit triggers, INSERT-into-CDC) because none of the EXPLAIN flavors
execute the inner write at the cluster level.

For the cases where measured stats on a write would be valuable, two
longer-term options remain on the table:

- **Transaction+rollback wrapper for writes**: revisit only if a
  user explicitly asks for ANALYZE-quality stats on DML and accepts
  the leak surface.
- **No-commit execution mode**: a new CRDB execution mode that runs
  the query with full semantics but never commits. Cleanest answer,
  but requires CRDB server changes.

### Schema Change Simulation

Extend EXPLAIN (DDL) with impact estimates: backfill size (estimated rows ×
column width), validation work (constraint checks), job creation (background
job count and type), and resource/duration estimates. This requires CRDB server
changes to expose table statistics and job cost models through EXPLAIN (DDL)
output.

### Agent-Safe Execution Mode

Guardrails for AI-generated queries beyond the basic safety model:
- Cost limits: reject queries with estimated cost above a configurable threshold
- Row limits: inject LIMIT on unbounded queries, with the limit configurable
- Mandatory EXPLAIN before execute: require agents to call `explain_sql` before
  `execute_sql`, enforced by the MCP server via session state
- Mutation budgets: limit the number of rows affected per session or per
  time window

### Version-Aware Validation

Validate SQL against a specific target CRDB version. This enables migration
tooling: verify that SQL written for v24.1 is compatible with v25.2 before
upgrading. Requires maintaining a mapping of syntax additions per CRDB version.

The foundation is already in place: every tool response includes a
`parser_version` field (derived from the cockroachdb-parser module version)
so consumers always know which CRDB grammar was used for parsing. Future work
would allow targeting a specific version via `--target-version v24.1`.

### Embedded SQL Extraction

Extract SQL from host-language source code (Go `db.Exec`/`db.Query` strings,
Python f-strings, TypeScript template literals). syntaqlite has experimental
support for Python and TypeScript. For the CRDB ecosystem, Go extraction is
the highest priority. Approaches: magic comments (`// crdb:sql`), tagged
template detection, or AST parsing of the host language.

### LSP for Editor Integration

A Language Server Protocol implementation sharing the validation engine with
the MCP server. Capabilities: diagnostics (real-time error highlighting),
completion (table/column/function names), hover (type information), formatting
(PrettyCfg), and code actions (apply fix suggestions). syntaqlite's LSP
demonstrates the full capability set. The validation engine is designed to be
shared between MCP and LSP — the semantic checker, schema loader, and error
enricher are transport-agnostic.

### Full Type Checking

Extend type checking beyond expressions to full query-level validation:
- SELECT result type inference (what types does this query return?)
- INSERT/UPDATE value type compatibility with target column types
- JOIN condition type compatibility
- Subquery result type checking
- User-defined type support (requires TypeResolver with connection)

### Integration with Migration Frameworks

Integration with popular migration tools (golang-migrate, goose, atlas) to
automatically load schema from migration files in the correct order. This
would enable `validate_sql` to work against a schema defined by a migration
history, not just a static schema dump.
