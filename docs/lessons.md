# Synthesized Design Lessons: Agent-Friendly SQL Tooling for CockroachDB

## Scope

This report synthesizes findings from a cross-vendor analysis of four SQL
tooling ecosystems — syntaqlite, pg_query, sqlc, and CockroachDB — across
25 research dimensions grouped into 7 clusters (Parser Architecture,
Schema & Semantic Analysis, Error Quality, Delivery & Integration,
Trust/Safety/Connection, Static Analysis/Linting, Simulation/Dry-Run,
plus CRDB-Specific dimensions). The goal is to extract ranked design
lessons for building an agent-friendly SQL validation tool delivered as
an MCP server and CLI, executable as a solo hackathon project (April 21-25,
2026).

**Input**: 88 vetted claims across 4 vendors (syntaqlite: 19, pg_query: 25,
sqlc: 22, crdb: 22). All claims have been through two rounds of adversarial
challenge review.

**Output**: 15 ranked design lessons, a recommended architecture, and a
day-by-day hackathon plan.

## Summary

The cross-vendor analysis reveals a consistent architectural pattern:
**real parser extraction + schema loading + semantic analysis + structured
output = agent-friendly SQL tooling**. Every external tool validates that
using the real database parser (not approximating the grammar) produces
dramatically better tooling. The gap across all tools is agent-friendly
output — errors are designed for humans, not programmatic consumers.

CockroachDB has the richest set of primitives (parser, type system, pgerror,
EXPLAIN modes) but the largest productization gap. The hackathon fills this
gap by wrapping existing primitives in an MCP server and CLI with structured
JSON output, adding a lightweight schema catalog, and populating the empty
builtins registry with metadata-only stubs.

The single most important finding is that the builtins registry in the
standalone extraction (cockroachdb-parser v0.25.2) is **EMPTY** — all 26
definition files are excluded because metadata and implementation closures
are interleaved. This is a BLOCKING dependency for function name validation,
type checking, return type inference, and autocomplete.

The second most important finding is that standalone type checking via
`MakeSemaContext(nil)` is significantly more capable than code reading
predicted — binary operators, CAST, COALESCE, CASE, NULLIF, and arrays
all type-check with zero configuration. Three code-reading predictions
were wrong, validating experiment-over-code-reading for implementation
claims.

## Ranked Lessons

### 1. Import cockroachdb-parser instead of extracting the parser from scratch

| Field | Value |
|-------|-------|
| **Rank** | 1 |
| **Applicability** | parser_wrapper |
| **Confidence** | high |
| **Recommendation** | adopt |

**Evidence basis**: syntaqlite achieves 99.7% parse fidelity with SQLite's
Lemon grammar. pg_query provides the real PostgreSQL parser as standalone C.
sqlc delegates to pg_query. cockroachdb-parser (v0.25.2) provides parser,
AST, builtins registry infrastructure, type system, PL/pgSQL, and pgerror
as a standalone Go module. Confirmed working via integration test.

**Why it may help**: Eliminates parser extraction as a hackathon task. Day 1
starts building value above the parser layer. The module includes six reusable
component areas via a single `go get` import. Integration test confirms 59
transitive deps and 9.6 MB module cache.

**Why it may not fit**: Version lag — v0.25.2 may be behind current CRDB
master. The 7 git patches in snapshot.sh may break with upstream changes. The
module maintainer has 5 unresponded external PRs.

---

### 2. Generate metadata-only builtins stubs as blocking prerequisite

| Field | Value |
|-------|-------|
| **Rank** | 2 |
| **Applicability** | semantic_checker |
| **Confidence** | high |
| **Recommendation** | adopt |

**Evidence basis**: The standalone extraction registers ZERO functions — all
26 definition files excluded by snapshot.sh. Register() is exported for
consumer-side registration. 876+ entries exist in the main CRDB repo. The
type checking experiment confirmed overload matching uses only
types.Equivalent() — zero context needed.

**Why it may help**: Unlocks function name validation with fuzzy-match
suggestions, overload resolution, return type inference, and function
autocomplete — all with zero user configuration. Combined with the
self-contained types package, enables substantial zero-config value that no
other studied tool provides offline.

**Why it may not fit**: Extracting 876+ function signatures requires parsing
Go source or manual extraction. tree.FunDefs (used by helpWithFunction())
requires the all_builtins.go subscription callback. Init ordering must be
verified.

**Open questions**:
- Should stubs live in a parser module fork or in consumer code?
- Can a code generator extract signatures from builtins.go reliably?
- How to keep stubs in sync with CRDB releases?

---

### 3. Load schema from CREATE TABLE files into a lightweight in-memory catalog

| Field | Value |
|-------|-------|
| **Rank** | 3 |
| **Applicability** | schema_loader |
| **Confidence** | high |
| **Recommendation** | adopt |

**Evidence basis**: syntaqlite uses TOML config with globs. sqlc uses YAML
config pairing schema and query paths with migration ordering. A CRDB catalog
prototype demonstrates viability in ~100 lines of Go, including
Levenshtein-distance suggestions for misspelled table/column names.

**Why it may help**: Transforms the tool from syntax-only to schema-aware
validation — the largest single capability jump. Dogfooding the CRDB parser
for schema loading ensures exact syntax compatibility with all CRDB DDL.

**Why it may not fit**: Handles CREATE TABLE only — CREATE INDEX, ALTER TABLE,
CREATE TYPE need additional parsing. The prototype is described in evidence
documentation but not independently verifiable on disk.

---

### 4. Design structured JSON error output with agent-actionable context

| Field | Value |
|-------|-------|
| **Rank** | 4 |
| **Applicability** | all |
| **Confidence** | high |
| **Recommendation** | adopt |

**Evidence basis**: syntaqlite has the best human-readable format but no JSON.
pg_query returns minimal C struct. sqlc outputs text. CRDB has pgerror with
SQLSTATE codes, severity, hint, detail, and constraint name — the strongest
foundation. Error position experiment confirmed reliable byte-offset positions
across single-line, multi-line, and multi-statement input.

**Why it may help**: Agent-friendly error output is the primary differentiator.
Each error with SQLSTATE code, line/column, category, available names, and fix
suggestions enables agents to auto-fix without parsing text.

**Why it may not fit**: Enriching errors with available columns/tables requires
the semantic checker to propagate more context than pgerror currently carries.
Semantic errors may not have the same position quality as parser errors.

---

### 5. Implement three-tier progressive capability unlock

| Field | Value |
|-------|-------|
| **Rank** | 5 |
| **Applicability** | mcp_server |
| **Confidence** | high |
| **Recommendation** | adopt |

**Evidence basis**: syntaqlite has two tiers (zero-config, schema-aware). sqlc
has two tiers (offline, connected). The pg_query ecosystem shows three tiers
across separate projects. CRDB delivers all three in one tool: (1) zero-config
offline, (2) schema-file offline, (3) connected.

**Why it may help**: Maps directly to the hackathon: Day 1 = Tier 1, Day 2 =
Tier 2, Day 3 = Tier 3. Each tier is independently useful. "Always show all
tools" is more agent-friendly than hiding capabilities.

**Why it may not fit**: Three tiers in 4 coding days is ambitious. Tier 3
may only be partially delivered. The "structured errors for missing
capability" pattern requires designing the error format up front.

---

### 6. Report all errors in a single pass for agent efficiency

| Field | Value |
|-------|-------|
| **Rank** | 6 |
| **Applicability** | semantic_checker |
| **Confidence** | high |
| **Recommendation** | adopt |

**Evidence basis**: syntaqlite explicitly finds ALL errors in one pass —
critical for agents who can fix all issues at once. sqlc validates all
queries. CRDB's optbuilder stops at first error.

**Why it may help**: Reduces the validate-fix-retry loop from N iterations to
potentially 1. For multi-statement files, reports all errors across all
statements.

**Why it may not fit**: Error recovery in semantic analysis is harder than in
parsing. After a name resolution failure, subsequent analysis may produce
cascading false errors.

---

### 7. Design MCP tool palette as a discoverable workflow

| Field | Value |
|-------|-------|
| **Rank** | 7 |
| **Applicability** | mcp_server |
| **Confidence** | medium |
| **Recommendation** | adopt |

**Evidence basis**: No external tool has an agent-oriented tool surface.
The CRDB MCP server has a greenfield opportunity. EXPLAIN (DDL) demonstrates
the discoverability problem — it exists but is buried in SQL syntax. Naming
it `explain_schema_change` makes it a first-class agent tool.

**Why it may help**: Tool ordering nudges agents toward safe patterns (validate
before execute). Named tools prevent agents from needing SQL syntax knowledge.

**Why it may not fit**: The ~20-tool catalog is a design proposal with no
implementation. The hackathon may deliver 5-8 core tools. Agent workflow
patterns are not well-established.

---

### 8. Surface EXPLAIN (DDL) as explain_schema_change for agents

| Field | Value |
|-------|-------|
| **Rank** | 8 |
| **Applicability** | mcp_server |
| **Confidence** | high |
| **Recommendation** | adopt |

**Evidence basis**: CRDB's EXPLAIN (DDL) surfaces declarative schema changer
plans with phases, element transitions, and mutation operations — confirmed
via live output samples. Three output modes: compact, SHAPE (best for agents),
VERBOSE. Unique among studied tools.

**Why it may help**: Gives agents the ability to predict schema change impact
before executing DDL. SHAPE mode output is compact and agent-suitable.

**Why it may not fit**: Requires live cluster connection (Tier 3). Output
needs a parser to convert tree-structured text to JSON. Does NOT provide
duration or resource consumption estimates.

---

### 9. Provide structured correction suggestions with replacement text

| Field | Value |
|-------|-------|
| **Rank** | 9 |
| **Applicability** | semantic_checker |
| **Confidence** | medium |
| **Recommendation** | adopt |

**Evidence basis**: syntaqlite provides text-only "did you mean?" for function
names. pg_query and sqlc provide no suggestions. The catalog prototype
demonstrates Levenshtein-distance suggestions. The builtins registry (once
populated) provides 876+ function names for fuzzy matching.

**Why it may help**: Transforms errors into actionable fixes. Structured
suggestions with replacement text, byte range, and confidence enable agents
to apply fixes automatically.

**Why it may not fit**: Structured suggestions require more development than
text hints. No edit-distance code exists in the CRDB codebase. Threshold
tuning requires experimentation.

---

### 10. Leverage MakeSemaContext(nil) for standalone type checking

| Field | Value |
|-------|-------|
| **Rank** | 10 |
| **Applicability** | semantic_checker |
| **Confidence** | high |
| **Recommendation** | adopt |

**Evidence basis**: Experimentation confirmed standalone type checking is
significantly more capable than code reading predicted. Binary operators,
CAST, COALESCE, CASE, NULLIF, arrays all type-check with MakeSemaContext(nil).
Only 1/18 test cases panicked (GREATEST). Three predictions were wrong.

**Why it may help**: Provides expression-level type checking with zero
configuration — unique among studied tools. Binary operator type mismatch
detection catches real bugs immediately.

**Why it may not fit**: GREATEST/LEAST panic needs recover() wrapper. Function
calls fail when builtins registry is empty. Full type checking scope in
complex multi-table queries is untested.

---

### 11. Deliver as a standalone binary using cockroachdb-parser module

| Field | Value |
|-------|-------|
| **Rank** | 11 |
| **Applicability** | all |
| **Confidence** | high |
| **Recommendation** | adopt |

**Evidence basis**: The parser import graph (94 internal deps, no KV/storage)
supports a standalone binary. Existing precedent: crlfmt (4.2 MB), optfmt
(5.3 MB). pg_query is standalone C. syntaqlite is standalone Rust.

**Why it may help**: Small binary enables fast downloads, CI/CD integration,
Homebrew distribution. No need to install the full cockroach binary.

**Why it may not fit**: Actual compiled binary size with all stubs is
unverified. If builtins stubs pull additional packages, size could grow.

---

### 12. Use stdio transport for local MCP and pgwire for cluster connection

| Field | Value |
|-------|-------|
| **Rank** | 12 |
| **Applicability** | mcp_server |
| **Confidence** | high |
| **Recommendation** | adopt |

**Evidence basis**: MCP spec's primary local transport is stdio. pgwire with
TLS is standard CockroachDB connection. mark3labs/mcp-go v0.46.0 supports
stdio and has been verified. Tool interface is portable between Go MCP SDKs.

**Why it may help**: Stdio is zero-config, inherently safe (no network
surface), and supported by all MCP clients. Dual-mode cleanly separates
offline from connected.

**Why it may not fit**: Stdio limits MCP to local use. Dual-mode transport
adds architectural complexity vs a single transport.

---

### 13. Implement read_only safety mode as default for cluster connections

| Field | Value |
|-------|-------|
| **Rank** | 13 |
| **Applicability** | mcp_server |
| **Confidence** | medium |
| **Recommendation** | adopt |

**Evidence basis**: syntaqlite, pg_query, and sqlc achieve safety by never
executing SQL. CRDB's tool goes beyond this by connecting to live clusters.
sql_safe_updates rejects dangerous statements. Read-only transactions
restrict mutations.

**Why it may help**: Prevents agents from accidentally mutating data. Safety
by construction (statement allowlists) makes the guarantee robust.

**Why it may not fit**: Read_only mode is Day 3 scope. Statement allowlisting
must be carefully enumerated (e.g., EXPLAIN ANALYZE executes the query).

---

### 14. Adopt YAML config with glob-based schema-to-query file mapping

| Field | Value |
|-------|-------|
| **Rank** | 14 |
| **Applicability** | cli |
| **Confidence** | high |
| **Recommendation** | adopt |

**Evidence basis**: syntaqlite (TOML) and sqlc (YAML) both use config files
with glob-based schema file mapping. The pattern converges across tools.

**Why it may help**: Glob-based mapping is simple and flexible. Directory-walk
discovery requires no explicit path argument. YAML is familiar in CRDB.

**Why it may not fit**: Config design is a Day 2 concern. For hackathon MVP,
command-line flags may be sufficient, deferring config to Day 4 polish.

---

### 15. Build detect_risky_query as deterministic static analysis over AST

| Field | Value |
|-------|-------|
| **Rank** | 15 |
| **Applicability** | mcp_server |
| **Confidence** | medium |
| **Recommendation** | consider |

**Evidence basis**: No external tool provides SQL risk detection. CRDB's AST
provides classification primitives (StatementType, CanModifySchema,
CanWriteData, ExtendedVisitor). Three proposed levels: AST-only,
catalog-aware, plan-aware.

**Why it may help**: Level 1 AST-only rules are cheap to implement and
immediately useful. Structured output with reason codes supports agent safety.

**Why it may not fit**: The rule architecture is a design proposal with no
implementation. Better suited as a Day 4 stretch goal than Day 2 priority.

## Recommended Architecture

### Parser Strategy

Import `cockroachdb-parser` v0.25.2 via `go get`. No parser extraction work.
The module provides parser, AST with Visitor/ExtendedVisitor, builtins
registry infrastructure (empty — requires stub generation), type system
(self-contained), pgerror (structured errors with SQLSTATE), PL/pgSQL
support, and help system.

### Schema Loading

Parse CREATE TABLE files using the CRDB parser itself (dogfooding). Build a
lightweight in-memory catalog mapping table names to column sets with types.
Support glob-based schema file mapping via YAML config or CLI flags. Handle
migration ordering for ALTER TABLE when feasible.

### Semantic Analysis Scope

Build a simplified semantic checker above the parser, not a stripped-down
optbuilder. Three analysis layers:
1. **Expression type checking** — MakeSemaContext(nil) for binary ops, CAST,
   COALESCE, CASE, NULLIF. Wrap GREATEST/LEAST in recover().
2. **Name resolution** — table/column existence checking against the
   lightweight catalog. Start with single-table queries, extend to JOINs.
3. **Function validation** — name checking, argument count, overload
   resolution against populated builtins registry.

Accumulate all errors in one pass rather than stopping at the first.

### Error Format

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
        {
          "replacement": "name",
          "range": {"start": 7, "end": 10},
          "confidence": 0.9
        }
      ]
    }
  ],
  "statement_type": "SELECT",
  "tier": "schema_file"
}
```

### Delivery Mechanism

Standalone Go binary (~10-30 MB) serving dual roles:
- **MCP server**: `crdb-sql-tool mcp` — stdio transport, JSON-RPC
- **CLI**: `crdb-sql-tool validate`, `crdb-sql-tool format`, `crdb-sql-tool
  check` — file/stdin input, `--output json` and `--output text` modes,
  `-e` flag for inline SQL

### MCP Tool Palette

Core tools ordered as a discoverable workflow:
1. `validate_sql` — parse + type check + name resolution (tiers 1-2)
2. `format_sql` — canonicalize SQL using FmtParsable (tier 1)
3. `parse_sql` — AST classification and fingerprinting (tier 1)
4. `explain_sql` — EXPLAIN output for DML (tier 3)
5. `explain_schema_change` — EXPLAIN (DDL) with SHAPE mode (tier 3)
6. `list_tables` / `describe_table` — catalog introspection (tier 2-3)
7. `detect_risky_query` — static risk analysis (stretch goal)

### Safety Model

Read-only by default for cluster connections. Three modes:
- `read_only` (default): SELECT, SHOW, EXPLAIN only. LIMIT injection.
  Statement timeouts. Read-only transaction wrapping.
- `safe_write` (future): Mutations with sql_safe_updates enabled. Requires
  explicit opt-in.
- `full_access` (future): Unrestricted. Requires explicit opt-in + warning.

Defense-in-depth: MCP-layer statement filtering + restricted DB user +
optional proxy.

### Progressive Capability

Three tiers in one tool:
- **Tier 1 (zero-config)**: Parse, format, classify, type check expressions,
  function help via builtins, statement risk detection. No setup required.
- **Tier 2 (schema-file)**: Name resolution, column type validation,
  INSERT/UPDATE checking, "did you mean?" suggestions. Requires schema
  files via config or CLI flags.
- **Tier 3 (connected)**: EXPLAIN, EXPLAIN (DDL), privilege checks, live
  schema introspection, execution. Requires cluster connection string.

All tools are always visible. Tools requiring a higher tier return structured
errors explaining what setup is needed to unlock the capability.

## Solo 5-Day Hackathon Plan

### Day 1 (Mon April 21) — Foundation: Tier 1 Zero-Config Value

**Goal**: Parse, format, classify, and type-check SQL with zero configuration.

| Deliverable | Details |
|-------------|---------|
| Go module setup | `go get` cockroachdb-parser v0.25.2. Confirm build. |
| MCP server skeleton | mark3labs/mcp-go v0.46.0, stdio transport. |
| `validate_sql` tool | Parse SQL, return structured errors with positions. |
| `format_sql` tool | FmtParsable for canonical formatting. |
| `parse_sql` tool | AST classification (DDL/DML/DCL/TCL), fingerprint. |
| Error format | JSON error envelope with SQLSTATE, line/column, severity. |
| Expression type checking | MakeSemaContext(nil) with recover() wrapper. |

**End-of-day test**: `validate_sql("SELECT 1 + 'hello'")` returns structured
type mismatch error with position, SQLSTATE code, and available types.

**Works if project stops here**: Yes. Zero-config SQL validation tool for
CRDB, useful for any agent generating CockroachDB SQL.

### Day 2 (Tue April 22) — Useful Layer: Tier 2 Schema-Aware Validation

**Goal**: Schema-file-based name resolution and suggestions.

| Deliverable | Details |
|-------------|---------|
| Schema loader | Parse CREATE TABLE files into lightweight catalog. |
| Name resolution | Table/column existence checking against catalog. |
| Suggestions | Levenshtein-distance matching for table/column/function names. |
| Multi-error reporting | Accumulate all errors, report in single response. |
| CLI skeleton | `crdb-sql-tool validate --schema schema.sql query.sql` |
| `list_tables` / `describe_table` | Catalog introspection tools. |

**End-of-day test**: `validate_sql("SELECT nme FROM users")` with schema file
returns "unknown column 'nme', did you mean 'name'?" with structured
suggestion including replacement text and byte range.

**Works if project stops here**: Yes. Offline schema-aware SQL validation
with suggestions. The most useful tier for agent workflows.

### Day 3 (Wed April 23) — Delivery Mechanism: Tier 3 Connected Features

**Goal**: Cluster-connected features with safety.

| Deliverable | Details |
|-------------|---------|
| Connection management | pgwire connection via connection string. |
| `explain_sql` tool | EXPLAIN for DML queries. |
| `explain_schema_change` tool | EXPLAIN (DDL) with SHAPE mode parsing. |
| Read-only safety | Statement allowlist, LIMIT injection, timeouts. |
| Live schema introspection | `list_tables` from information_schema. |
| CLI `--dsn` flag | Connection string for connected features. |

**End-of-day test**: `explain_schema_change("ALTER TABLE users ADD COLUMN age INT NOT NULL DEFAULT 0")`
returns structured JSON with phases, backfill operations, and estimated steps.

### Day 4 (Thu April 24) — Stretch Goals

**Goal**: Differentiated features if core is solid.

| Deliverable | Priority |
|-------------|----------|
| Builtins registry stubs | High — unblocks function validation |
| `detect_risky_query` tool | Medium — Level 1 AST-only rules |
| `summarize_sql` tool | Medium — statement classification + summary |
| YAML config file | Low — glob-based schema mapping |
| `simulate_sql` tool | Low — transaction-rollback wrapper |

### Day 5 (Fri April 25) — Polish Only

**Goal**: Demo-ready polish. No new features.

| Task | Details |
|------|---------|
| README | Installation, quick start, tool catalog |
| Demo script | Walkthrough showing tier progression |
| Error message review | Consistency, clarity, agent-friendliness |
| Edge case testing | Multi-statement, PL/pgSQL, CRDB extensions |
| Binary build | goreleaser config, verify binary size |

### Critical Path and Biggest Risk

**Critical path**: Day 1 MCP SDK integration -> Day 2 schema loader ->
Day 3 connection management.

**Biggest risk**: Lightweight catalog scope creep on Day 2. CREATE TABLE
parsing is straightforward for simple tables but complex DDL (computed
columns, foreign keys, partial indexes) could consume the day. Mitigation:
start with table names and column names/types only, add constraints
iteratively.

**Second risk**: Builtins stub generation complexity. If generating 876+
function metadata stubs proves too complex for Day 4, the tool still works
for parsing, formatting, and schema-aware validation — function-level
checking is deferred.

## Design Patterns Observed

### 1. Real Parser Extraction

All three external tools validate that using the real database parser produces
dramatically better tooling than approximating the grammar. syntaqlite extracts
SQLite's Lemon grammar (99.7% fidelity). pg_query extracts PostgreSQL's parser
via copy-and-patch. sqlc delegates to pg_query. The pattern is universal:
**never approximate the grammar**.

### 2. Parser-as-Foundation

pg_query demonstrates the parser-as-foundation pattern: provide a stable
serialization format and let an ecosystem build semantic layers on top. The
progression pg_query -> sqlc -> pganalyze shows how a parser library enables
increasingly sophisticated tools. CRDB should deliver all three layers
(parser, schema analysis, connected features) in one tool rather than three
separate projects.

### 3. Progressive Capability Unlock

syntaqlite (two tiers) and sqlc (two tiers) both implement progressive
capability models. The pattern is consistent: zero-config parsing is the
baseline, schema-file loading adds validation depth, and live connections
add execution capabilities. The key insight for agents: always show all
tools, return structured errors when a capability layer is missing rather
than hiding tools.

### 4. Schema from DDL Files

syntaqlite and sqlc converge on the same schema loading pattern: parse CREATE
TABLE SQL files with config-driven file mapping. This is the clear model.
Using the CRDB parser to parse CRDB DDL files provides exact syntax
compatibility — an advantage no external tool has.

### 5. Structured Over Human-Readable

Every external tool produces errors designed for human consumption.
syntaqlite has the best human format (multi-error, carets, help text) but
no JSON. sqlc has text with file paths. pg_query has a minimal C struct.
The consistent gap is machine-readable, agent-actionable error output.
This is the primary opportunity for the CRDB tool.

### 6. Experiment Over Code Reading

The type checking experiment revealed that three code-reading predictions
were wrong. MakeSemaContext(nil) is more capable than expected. This
validates a key research methodology: for implementation claims about
standalone viability, run experiments rather than relying on code reading
alone. The experiment discovered a "hidden gem" that code reading missed.

## Open Questions

1. **Builtins stub generation**: Can a code generator reliably extract 876+
   function signatures from builtins.go and related files? What format
   should stubs use? Should they live in a parser module fork or consumer
   code?

2. **EXPLAIN (DDL) parsing**: Can the tree-structured text output (with
   Unicode box-drawing characters) be parsed into structured JSON in a
   few hours? JSON params use doubled quotes in cockroach sql output.

3. **Error recovery in semantic analysis**: How to handle cascading false
   errors after a name resolution failure? What "error state" representation
   prevents misleading downstream diagnostics?

4. **MCP tool count for MVP**: How many of the ~20 proposed tools should the
   hackathon expose? Over-designing the palette could consume implementation
   time.

5. **EXPLAIN ANALYZE in read_only mode**: Should it be allowed? It executes
   the query to collect runtime statistics. SET statements and session
   configuration also need policy decisions.

6. **Edit-distance threshold**: What Levenshtein threshold minimizes false
   positives for name suggestions? Does the threshold differ between table
   names (short, unique) and function names (long, many similar)?

7. **cockroachdb-parser versioning**: v0.25.2 may lag current CRDB master.
   How to handle version drift? The single maintainer has 5 unresponded
   external PRs.

8. **Binary size**: Actual compiled binary size with populated builtins stubs
   is unknown. The 59-dep module is lightweight but stubs may pull additional
   packages.

## Suggested Next Evidence

1. **Builtins stub prototype**: Write a small Go program that extracts
   function metadata (name, arg types, return type, volatility) from
   CRDB's builtins.go files. Test that Register() with metadata-only
   stubs enables function name validation and overload resolution.

2. **EXPLAIN (DDL) parser**: Build a prototype parser for EXPLAIN (DDL)
   output that converts the tree-structured text into structured JSON.
   Test against the three confirmed output samples (ADD COLUMN, DROP INDEX
   CASCADE, ALTER PRIMARY KEY).

3. **MCP prototype**: Build a minimal MCP server with 3 tools (validate_sql,
   format_sql, parse_sql) using mark3labs/mcp-go and test with Claude Code.
   Verify stdio transport, tool discovery, and structured error output.

4. **Schema loader edge cases**: Test the lightweight catalog prototype
   against complex DDL: multi-column PRIMARY KEY, UNIQUE constraints with
   names, REFERENCES (foreign keys), computed columns, partial indexes,
   CRDB-specific features (USING HASH, regional tables).

5. **Error position coverage**: Extend the error position experiment to
   semantic errors from the new checker. Verify that name resolution and
   type checking errors include accurate positions recovered from the
   original SQL text.

6. **Multi-error recovery**: Experiment with error recovery strategies in
   the semantic checker. Test that name resolution failures don't produce
   cascading false errors in subsequent analysis phases.

7. **cockroachdb-parser v0.25.2 vs v0.25.3**: Verify that the type checking
   experiment results (run on v0.25.3) reproduce on the published v0.25.2
   Go module proxy version.
