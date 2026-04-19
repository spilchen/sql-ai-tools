# "Agent-Friendly SQL Tooling for CockroachDB" — Solo Hackathon Plan

## The Pitch

Give AI agents the same SQL understanding that CockroachDB itself has.
CockroachDB has a world-class SQL parser, type system, and error infrastructure
— but it is all trapped inside the server binary. This project wraps those
primitives in an MCP server and CLI that lets agents validate, explain, and
safely execute SQL without parsing error strings or guessing at syntax. The
result: agents that write correct CockroachDB SQL on the first try.

## Problem Statement

AI agents write SQL for CockroachDB constantly — and get it wrong in subtle
ways that a real parser would catch instantly. Today's workflow is: generate
SQL, send it to a running cluster, parse the error string, guess at a fix,
retry. This loop is slow, expensive, and fragile.

**What agents need but don't have:**

- **Structured errors**: Agents parse English error strings today. They need
  JSON with SQLSTATE codes, positions, available columns, and fix suggestions
  — enough context to self-correct without another LLM call.
- **Schema awareness without a cluster**: Agents should validate table/column
  references against schema files offline, catching misspelled names before
  hitting the network.
- **Real parser fidelity**: Generic SQL linters miss CRDB-specific syntax
  (hash-sharded indexes, regional tables, PL/pgSQL). Only the real CRDB parser
  catches exactly the errors CockroachDB would catch.
- **Discoverable capabilities**: CRDB already has EXPLAIN (DDL) for schema
  change impact analysis, but it is buried in SQL syntax. Agents don't know it
  exists. Named tools (`explain_schema_change`) make capabilities discoverable.

**Why CockroachDB is uniquely positioned:**

The primitives already exist. The parser (`cockroachdb-parser` Go module), type
system (`pkg/sql/types`), builtins registry infrastructure, structured errors
(`pgerror` with SQLSTATE codes), and AST walking (`Visitor`/`ExtendedVisitor`)
are all available as a standalone Go module. The gap is not capability — it is
productization.

## Demo Vision

**Demo day scenario**: An AI agent (Claude Code) writing SQL for CockroachDB.

1. Agent generates `SELECT nme FROM users WHERE id = 1` — a typo.
2. The MCP `validate_sql` tool catches the error *before it hits the cluster*
   and returns structured JSON:
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
3. Agent reads the structured suggestion, applies the fix automatically — no
   second LLM call needed.
4. Agent generates a DDL change. The `explain_schema_change` tool shows the
   declarative schema changer plan with phases and backfill operations — the
   agent knows the impact before executing.
5. All of this works with schema files alone — no running cluster required for
   steps 1-3.

**The punchline**: The agent goes from 5 retry loops to 1. Validation time
drops from seconds (round-trip to cluster) to milliseconds (local parse).
Error recovery goes from "parse English, guess" to "read JSON, apply fix."

## Architecture

### Parser Strategy

Import `cockroachdb-parser` v0.25.2 via `go get`. No parser extraction work.
The module provides six reusable component areas: parser, AST with
Visitor/ExtendedVisitor, builtins registry infrastructure (empty — requires
stub generation), self-contained type system, pgerror with SQLSTATE codes, and
help system with per-statement syntax documentation. Confirmed working via
integration test (59 transitive deps, 9.6 MB module cache).

*Lesson source*: All three external tools (syntaqlite, pg_query, sqlc) validate
that using the real database parser — not approximating the grammar — produces
dramatically better tooling. syntaqlite achieves 99.7% parse fidelity with
SQLite's Lemon grammar. pg_query provides the real PostgreSQL parser as
standalone C. CRDB already has this solved.

### Schema Loading

Parse CREATE TABLE files using the CRDB parser itself (dogfooding ensures exact
syntax compatibility with all CRDB DDL including extensions). Build a
lightweight in-memory catalog mapping table names to column sets with types,
nullability, defaults, primary keys, and indexes. A catalog prototype
demonstrates this is viable in ~100 lines of Go with Levenshtein-distance
"did you mean?" suggestions.

*Lesson source*: syntaqlite (TOML config with globs) and sqlc (YAML config
pairing schema/query paths) converge on the same pattern: parse DDL files,
build in-memory catalog, validate queries against it.

### Semantic Analysis Scope

Build a simplified semantic checker above the parser, not a stripped-down
optbuilder (the optbuilder is not extractable due to deep optimizer
dependencies). Three analysis layers:

1. **Expression type checking** — `MakeSemaContext(nil)` for binary operators,
   CAST, COALESCE, CASE, NULLIF. Wrap GREATEST/LEAST in `recover()`. The type
   checking experiment confirmed this is significantly more capable than code
   reading predicted — a hidden gem unique among studied tools.
2. **Name resolution** — table/column existence checking against the
   lightweight catalog. Start with single-table queries, extend to JOINs.
3. **Function validation** — name checking, argument count, overload
   resolution against populated builtins registry (once stubs exist).

Accumulate all errors in one pass rather than stopping at the first — critical
for agent consumers who can fix all issues at once.

### Error Output Format

JSON error envelope wrapping pgerror with agent-specific context:

```json
{
  "errors": [
    {
      "code": "42703",
      "severity": "ERROR",
      "message": "column \"nme\" does not exist",
      "position": {"line": 1, "column": 8, "byte_offset": 7},
      "category": "unknown_column",
      "available": ["name", "email", "id"],
      "suggestions": [
        {"replacement": "name", "range": {"start": 7, "end": 10}, "confidence": 0.9}
      ]
    }
  ],
  "statement_type": "SELECT",
  "tier": "schema_file",
  "parser_version": "v25.2.5"
}
```

*Lesson source*: Every external tool has a gap in agent-friendly error output.
syntaqlite produces the best human-readable format but no JSON. pg_query
returns a minimal C struct. sqlc outputs text. CRDB's pgerror (SQLSTATE codes,
severity, hint, detail) is the strongest foundation — the MCP tool enriches it
with available names, expected types, and structured fix suggestions.

### MCP Server Tool Palette

Tools are presented as an ordered palette that teaches a workflow — not a bag
of unrelated capabilities. The ordering nudges agents toward safe patterns:
validate before explain, explain before execute.

| # | Tool | Tier | Description |
|---|------|------|-------------|
| 1 | `validate_sql` | 1-2 | Parse + type check + name resolution. The primary tool. |
| 2 | `explain_sql` | 3 | EXPLAIN output for DML queries with structured JSON. |
| 3 | `explain_schema_change` | 3 | EXPLAIN (DDL) with SHAPE mode — predict DDL impact. |
| 4 | `simulate_sql` | 3 | Execute in read-only transaction, return results without side effects. |
| 5 | `execute_sql` | 3 | Execute with safety guardrails. |
| 6 | `format_sql` | 1 | Canonicalize SQL using FmtParsable. Supports `--color` for syntax-highlighted terminal output. Auto-strips `cockroach sql` shell prompts and continuation markers (`root@...>`, `->`) from pasted input. |
| 7 | `parse_sql` | 1 | AST classification (DDL/DML/DCL/TCL), fingerprinting, and parser version tag. |
| 8 | `list_tables` | 2-3 | Enumerate tables from loaded schema or live catalog. |
| 9 | `describe_table` | 2-3 | Column names, types, constraints, indexes for a table. |

**Key insight**: SQL is the assembly language; the agent tool is the product.
EXPLAIN (DDL) already exists in CRDB but is buried in SQL syntax. Naming it
`explain_schema_change` makes it a first-class discoverable tool. Not all tools
need to be in the hackathon MVP, but the palette design shows the full vision.

*Lesson source*: No external tool has an agent-oriented tool surface. pg_query
is a C library. sqlc is a batch CLI. syntaqlite integrates via LSP, not MCP.
This is greenfield.

### CLI Interface

Mirror the MCP tools as CLI subcommands:

```bash
# Validate SQL (file, stdin, or inline)
crdb-sql validate schema.sql query.sql
crdb-sql validate --schema schema.sql -e "SELECT nme FROM users"
echo "SELECT 1" | crdb-sql validate

# Format SQL
crdb-sql format -e "select   id,name from users where id=1"

# Format with syntax highlighting
crdb-sql format --color -e "select   id,name from users where id=1"

# Format pasted cockroach sql shell output (prompts auto-stripped)
crdb-sql format <<'EOF'
root@:26257/defaultdb> SELECT id, name
                    -> FROM users
                    -> WHERE id = 1;
EOF

# Explain DDL impact (connected)
crdb-sql explain-ddl --dsn "postgresql://..." -e "ALTER TABLE users ADD COLUMN age INT"

# Output modes
crdb-sql validate --output json schema.sql query.sql
crdb-sql validate --output text schema.sql query.sql
```

### Simulation Depth

What is achievable at each tier:

| Layer | Standalone | Connected |
|-------|-----------|-----------|
| **Parse + validate syntax** | Yes — zero config | N/A |
| **Type check expressions** | Yes — MakeSemaContext(nil) | N/A |
| **Name resolution** | Yes — with schema files | Yes — live catalog |
| **EXPLAIN (plan shape)** | No | Yes |
| **EXPLAIN (DDL) impact** | No | Yes — SHAPE mode |
| **Simulate execution** | No | Stretch — txn+rollback |
| **Execute** | No | Stretch — with guardrails |

### Safety Model

Three modes with defense-in-depth:

- **`read_only`** (default): SELECT, SHOW, EXPLAIN only. Statement allowlist
  enforced at the MCP layer. LIMIT injection on unbounded queries. Statement
  timeouts. Read-only transaction wrapping.
- **`safe_write`** (future): Mutations with `sql_safe_updates` enabled.
  Required WHERE clauses for DELETE/UPDATE. Row caps. Requires explicit opt-in
  via configuration.
- **`full_access`** (future): Unrestricted. Requires explicit opt-in +
  confirmation warning.

**Defense-in-depth** (three independent layers):
1. MCP-layer statement filtering (application code)
2. Restricted database user (DB-level permissions)
3. Optional proxy (network-level filtering)

Even if one layer has a bug, the others catch it.

### Progressive Capability

Three tiers in one tool — useful at zero setup, powerful with a connection:

| Tier | Setup Required | Capabilities |
|------|---------------|-------------|
| **1: Zero-config** | None | Parse, format, classify, type check expressions, function help (with stubs), statement risk detection, fingerprinting |
| **2: Schema-file** | `--schema schema.sql` | Name resolution, column type validation, INSERT/UPDATE checking, "did you mean?" suggestions |
| **3: Connected** | `--dsn postgresql://...` | EXPLAIN, EXPLAIN (DDL), privilege checks, live schema introspection, execution |

**All tools are always visible.** Tools requiring a higher tier return
structured errors explaining what setup unlocks the capability:

```json
{
  "error": "connection_required",
  "message": "explain_sql requires a database connection",
  "hint": "Pass --dsn or set CRDB_DSN to enable connected features",
  "available_offline": ["validate_sql", "format_sql", "parse_sql"]
}
```

*Lesson source*: syntaqlite has two tiers (zero-config, schema-aware). sqlc has
two tiers (offline, connected). The pg_query ecosystem shows three tiers across
separate projects. CRDB delivers all three in one tool. The "always show all
tools" pattern is more agent-friendly than hiding capabilities.

### Connection Management

- **Configuration**: Endpoint, database, sslmode specified via `--dsn` flag,
  config file, or `CRDB_DSN` environment variable.
- **Secrets**: Connection credentials via environment variables (`CRDB_DSN`)
  or OS keychain. Never stored in plain config files.
- **Status**: Connection status always visible via a `connection_status` field
  in tool responses, so agents know which tier they are operating in.

### Rule-Based Analysis

`detect_risky_query` and `summarize_sql` are deterministic static analysis
tools built over AST, catalog metadata, and plan output. No LLM needed for
safety-critical judgment.

**Rule registry** with conditions, severity, messages, and fix hints:

| Rule | Level | Severity | Condition |
|------|-------|----------|-----------|
| DELETE without WHERE | AST-only | critical | Delete node has nil Where |
| UPDATE without WHERE | AST-only | critical | Update node has nil Where |
| DROP TABLE | AST-only | critical | StatementType() == DDL, tag "DROP TABLE" |
| SELECT * | AST-only | low | Star expression in select list |
| Large table mutation | Catalog-aware | high | Mutation targets table with >1M rows |
| Full table scan | Plan-aware | medium | EXPLAIN shows full scan on large table |

**Three levels matching the progressive capability model:**
1. **AST-only** (offline): Statement classification, pattern matching
2. **Catalog-aware** (schema files): Table size awareness, sensitive tables
3. **Plan-aware** (connected): Real cost estimates from EXPLAIN

All rules produce structured output with reason codes, severity, and fix hints.
Rules are testable, explainable, and produce machine-readable reason codes.

## 5-Day Solo Plan

### Day 1 (Monday, April 21): Foundation — Tier 1 Zero-Config Value

**Deliverables:**
- Go module setup: `go get cockroachdb-parser@v0.25.2`, confirm build
- MCP server skeleton: mark3labs/mcp-go, stdio transport
- `validate_sql` tool: parse SQL, return structured errors with SQLSTATE codes,
  line/column positions, severity
- `format_sql` tool: canonicalize SQL using FmtParsable
- `parse_sql` tool: AST classification (DDL/DML/DCL/TCL), statement tag,
  query fingerprinting (FmtHideConstants), parser version tag (derived from
  cockroachdb-parser module version, e.g., "v25.2.5")
- JSON error envelope format: SQLSTATE, line/column, severity, category
- Expression type checking: MakeSemaContext(nil) with recover() wrapper for
  binary operator type mismatch detection

**End-of-day test:**
```
validate_sql("SELECT 1 + 'hello'")
→ structured type mismatch error with position, SQLSTATE 42804, and description
```

**What works if project stops here:** A zero-config SQL validation tool for
CockroachDB. Agents can validate any CRDB SQL syntax, format queries, classify
statements, and catch expression type mismatches — all without a cluster or
schema files.

**Technical risks:**
- MCP SDK integration issues (mitigated: mcp-go verified via prototype evaluation)
- Error position extraction from pgerror Detail field (mitigated: experiment
  confirmed simple string processing works)

---

### Day 2 (Tuesday, April 22): Core Value — Tier 2 Schema-Aware Validation

**Deliverables:**
- Schema loader: parse CREATE TABLE files into lightweight in-memory catalog
  (table names, column names/types/nullability/defaults, primary keys, indexes)
- Name resolution: table/column existence checking against loaded catalog
- Suggestions: Levenshtein-distance "did you mean?" for table, column, and
  function names with structured suggestion objects (replacement text, byte
  range, confidence)
- Multi-error reporting: accumulate all errors across all statements, report in
  single response
- CLI skeleton: `crdb-sql validate --schema schema.sql query.sql` with
  `--output json` and `--output text` modes
- `list_tables` / `describe_table` tools: catalog introspection from loaded
  schema files

**End-of-day test:**
```
validate_sql("SELECT nme FROM users") with schema containing users(id, name, email)
→ "unknown column 'nme', did you mean 'name'?" with structured suggestion
  {replacement: "name", range: {start: 7, end: 10}, confidence: 0.9}
```

**What works if project stops here:** Offline schema-aware SQL validation with
structured suggestions. This is the most useful tier for agent workflows — agents
can validate SQL against schema files without a running cluster, get structured
fix suggestions, and auto-correct errors.

**Technical risks:**
- Schema loader scope creep: complex DDL (computed columns, foreign keys,
  partial indexes) could consume the day. **Mitigation**: start with table names
  and column names/types only, add constraints iteratively
- Levenshtein library selection and threshold tuning. **Mitigation**: use
  established Go library, start with threshold of 3

---

### Day 3 (Wednesday, April 23): Delivery — Tier 3 Connected Features

**Deliverables:**
- Connection management: pgwire connection via `--dsn` flag or `CRDB_DSN`
  environment variable
- `explain_sql` tool: EXPLAIN for DML queries with structured JSON output
- `explain_schema_change` tool: EXPLAIN (DDL) with SHAPE mode, parsed into
  structured JSON with phases, element transitions, and operations
- Read-only safety: statement allowlist (SELECT, SHOW, EXPLAIN only), LIMIT
  injection, statement timeouts, read-only transaction wrapping
- Live schema introspection: `list_tables` / `describe_table` from
  `information_schema` when connected (falling back to loaded schema files)
- CLI `--dsn` flag for connected features

**End-of-day test:**
```
explain_schema_change("ALTER TABLE users ADD COLUMN age INT NOT NULL DEFAULT 0")
→ structured JSON with phases (StatementPhase, PreCommitPhase, PostCommitPhase),
  backfill operations, and element transitions
```

**What works if project stops here:** Full three-tier tool: zero-config parsing,
schema-file validation with suggestions, and connected EXPLAIN/DDL analysis.
This is a complete hackathon deliverable.

**Technical risks:**
- EXPLAIN (DDL) output parser: tree-structured text with Unicode box-drawing
  characters needs a parser to convert to JSON. **Mitigation**: focus on SHAPE
  mode (simpler, natural language keywords) first; compact mode parser is a
  stretch
- Connection management edge cases (TLS, auth). **Mitigation**: use standard
  pgx library, accept `--dsn` as a full connection string

---

### Day 4 (Thursday, April 24): Stretch Features

**Deliverables (in priority order):**

| Feature | Priority | Rationale |
|---------|----------|-----------|
| Builtins registry stubs | High | Unblocks function name validation, overload resolution, return type inference. Auto-generate metadata-only stubs for ~876+ functions |
| `detect_risky_query` tool | Medium | Level 1 AST-only rules: DELETE without WHERE, DROP TABLE, UPDATE without WHERE. Structured output with reason codes and severity |
| `summarize_sql` tool | Medium | Statement classification + structured summary (operation, tables, columns, predicates) |
| YAML config file | Low | Glob-based schema-to-query file mapping for projects |
| `simulate_sql` tool | Low | Transaction-rollback wrapper for execute-without-side-effects |

**What works if project stops here:** All Day 1-3 deliverables plus function
validation (if stubs completed) and risk detection (if rules completed). Each
stretch feature is independently useful.

---

### Day 5 (Friday, April 25): Polish + Demo Prep

**No new feature work.**

**Demo preparation:**
- Build a demo script showing the three-tier progression: zero-config →
  schema-file → connected
- Prepare 3-4 compelling SQL examples that showcase structured error output,
  suggestions, and EXPLAIN (DDL)
- Record or script the demo for async viewers

**Final polish:**
- README with installation instructions, quick start, and tool catalog
- Error message consistency review across all tools
- Edge case testing: multi-statement input, PL/pgSQL, CRDB-specific syntax
  (hash-sharded indexes, regional tables)
- Binary build: goreleaser config, verify binary size (~10-30 MB target)
- Test with Claude Code as the actual agent consumer

## Technical Risks

| # | Risk | Severity | Mitigation |
|---|------|----------|------------|
| 1 | Schema loader scope creep — complex DDL consumes Day 2 | High | Start with table names + column names/types only. Add constraints iteratively. CREATE TABLE only — defer ALTER TABLE, CREATE INDEX |
| 2 | Builtins stub generation complexity — extracting 876+ function signatures from interleaved Go source | High | Defer to Day 4 stretch. Tool works for parsing, formatting, and schema-aware validation without function-level checking |
| 3 | EXPLAIN (DDL) output parsing — tree-structured text with Unicode box-drawing chars | Medium | Focus on SHAPE mode first (simpler, natural language keywords). Compact mode parser is optional |
| 4 | cockroachdb-parser version lag — v0.25.2 may miss recent syntax additions | Medium | v0.25.2 covers the vast majority of CRDB SQL. Version-specific features are edge cases for hackathon scope |
| 5 | MCP SDK integration issues with mcp-go | Low | SDK verified via prototype evaluation. Tool interface is portable to official go-sdk if needed |
| 6 | Binary size growth from builtins stubs | Low | Even if stubs add 5-10 MB, the binary stays under 40 MB — still 10x smaller than full cockroach binary |

**Critical path**: Day 1 MCP SDK integration → Day 2 schema loader → Day 3
connection management. Each day builds on the previous day's foundation. Each
day also delivers a standalone-useful tool.

## The Meta Angle

This project uses AI coding tools to build a tool that makes AI coding tools
better at CockroachDB. The recursive loop writes itself for demo day:

> "I used Claude Code to build the MCP server. Now Claude Code uses the MCP
> server to write better CockroachDB SQL. The tool I built with AI makes AI
> better at the thing I built the tool for."

The meta angle is also the validation strategy: if the tool makes Claude Code
measurably better at writing CockroachDB SQL during development, it works.

## What Makes This Different

This is not "yet another SQL linter."

- **It uses the real CRDB parser.** It catches exactly the errors CockroachDB
  would catch — nothing more, nothing less. No grammar approximation, no
  incomplete dialect support, no false positives from a generic SQL parser.
- **It produces structured output for agents.** Every error includes SQLSTATE
  codes, positions, available alternatives, and fix suggestions in JSON — not
  error strings that agents have to parse with regex.
- **It works offline.** Schema-aware validation from files, not a running
  cluster. Agents get fast, local feedback.
- **It surfaces hidden capabilities.** EXPLAIN (DDL) already exists in CRDB
  but is undiscoverable by agents. Named tools make it a first-class operation.
- **It is safe by default.** Read-only mode with statement allowlists and
  defense-in-depth. An agent cannot accidentally DROP TABLE.
- **It is progressive.** Useful with zero setup. More powerful with schema
  files. Most powerful with a connection. Always honest about what tier you are
  operating in.
