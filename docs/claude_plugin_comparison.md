# sql-ai-tools vs. cockroachdb/claude-plugin

## Why this doc exists

CRL shipped [`cockroachdb/claude-plugin`](https://github.com/cockroachdb/claude-plugin)
in parallel with the development of `sql-ai-tools`. Both projects describe
themselves as "CockroachDB tooling for Claude," so the question
*"isn't this redundant?"* will come up. This doc is the answer: an honest
side-by-side, what's shared, what's distinct, and why both should exist.

## What each project actually is

### `cockroachdb/claude-plugin`

A **Claude-Code marketplace plugin**. Installed via
`/install-plugin cockroachdb`. It does not run any of its own SQL logic; it
wires Claude Code to existing CRL infrastructure and ships agent-facing
content.

Components:

- **MCP backends (proxied, not implemented here)**:
  - `cockroachdb-toolbox` — Google's `mcp-toolbox` binary, run locally
    against a real cluster. Tools: `cockroachdb-execute-sql`,
    `cockroachdb-list-schemas`, `cockroachdb-list-tables`.
  - `cockroachdb-cloud` — CRL's managed MCP server (OAuth/API key) for
    CockroachDB Cloud. 8 read tools (`list_clusters`, `get_cluster`,
    `list_databases`, `list_tables`, `get_table_schema`, `select_query`,
    `explain_query`, `show_running_queries`) plus 3 consent-gated write
    tools (`create_database`, `create_table`, `insert_rows`) — 11 total.
- **Sub-agents**: `cockroachdb-dba`, `cockroachdb-developer`,
  `cockroachdb-operator` — markdown personas with curated CRDB best-practice
  prose (PK strategy, retry loops, set-based ops, etc.).
- **Skills**: ~10 domains sourced from the `cockroachdb-skills` submodule
  (query and schema design, observability, security, migrations, ops, …).
- **Hooks** (Python 3, no deps):
  - `validate-sql` (PreToolUse on the Toolbox backend's
    `cockroachdb-execute-sql` tool only — Cloud MCP read/write tools are
    not covered) — regex over the SQL string; blocks `DROP DATABASE` /
    `TRUNCATE`, warns on `SERIAL` and multi-DDL transactions.
  - `check-sql-files` (PostToolUse on `Write`/`Edit`/`MultiEdit`) —
    anti-pattern linter against SQL files on disk.
- References (does not bundle) the `ccloud` CLI for cluster lifecycle ops.

**Every MCP tool requires a live cluster.**

### `sql-ai-tools` (this repo)

A **standalone Go binary** (`crdb-sql`) that is both a CLI and an MCP server.
Built on top of `cockroachdb-parser` — the real CRDB parser, AST, type
system, and `pgerror` infrastructure embedded in-process.

Components (per `README.md` and `docs/design_doc.md`):

- Tools: `validate_sql`, `format_sql`, `parse_sql`, `list_tables`,
  `describe_table`, `explain_sql` (auto-dispatches DDL via `EXPLAIN
  (DDL, SHAPE)`), `detect_risky_sql`, `summarize_sql`, `simulate_sql`,
  `execute_sql`.
- A **three-tier capability model**:
  - **Tier 1 — Zero-config**: parse, format, classify, type-check
    expressions, fingerprint, detect risky patterns. Fully offline.
  - **Tier 2 — Schema files**: load `CREATE TABLE` files into a
    lightweight catalog for name resolution, column-type checking, and
    Levenshtein "did you mean?" suggestions. No cluster needed.
  - **Tier 3 — Connected**: EXPLAIN, schema-change plans, and (planned)
    guarded execution against a live cluster.
- **Structured JSON errors** with SQLSTATE, severity, line/column position,
  byte offset, error category, available alternatives (for unknown-name
  errors), expected types (for type mismatches), and replacement
  suggestions with byte ranges and confidence scores.
- A rule-based risk/summary engine over the AST.

## What they have in common

1. Same north-star user — an AI agent writing SQL for CockroachDB inside
   Claude.
2. MCP as the integration surface.
3. Functional overlap on Tier 3: `list_tables`, table description, EXPLAIN,
   and SQL execution exist in both worlds.
4. Both default to read-only with a guardrail layer in front of execution
   (claude-plugin's PreToolUse Python hook; sql-ai-tools' `read_only` /
   `safe_write` modes plus AST-level statement allowlists).
5. Both are CRL-owned, Apache-2.0.

## Where they meaningfully differ

| Axis | `claude-plugin` | `sql-ai-tools` |
|---|---|---|
| **Shape** | Claude-Code plugin (skills + agents + hooks + MCP config bundle) | Plain MCP server + CLI binary |
| **Client coupling** | Claude Code only (marketplace plugin) | Any MCP client; also runnable from CI/shell |
| **Backend** | Proxies to *external* servers (`mcp-toolbox` + Cloud MCP) | Embeds the CRDB parser **in-process**; no external server |
| **Cluster requirement** | **Required** for every MCP tool | **Optional** — Tier 1/2 work fully offline |
| **Parser fidelity** | None of its own — relies on the cluster to parse and evaluate | Real `cockroachdb-parser` AST; CRDB-specific syntax (hash-sharded indexes, regional tables, PL/pgSQL) understood without a cluster |
| **Validation depth** | `validate-sql` is a Python regex on the SQL string | Full parse → type-check → name-resolve → enriched JSON diagnostics with positions, available names, did-you-mean fixes |
| **Schema-file workflow** | Not supported | First-class — load `CREATE TABLE` files, validate queries against them with no cluster |
| **Cloud control plane** | Yes — cluster lifecycle, backups, audit, networking via Cloud MCP + `ccloud` | Out of scope (SQL-level only) |
| **Curated CRDB knowledge** | Substantial — 3 sub-agents + ~10 skill domains | None shipped |
| **Distribution** | One-click `/install-plugin cockroachdb` | `make build` + `claude mcp add` |
| **Linter style** | Regex over SQL string / file contents | AST-based rule engine over arbitrary statements |

## The honest read

**Is `sql-ai-tools` made obsolete by `claude-plugin`?** No. The headline
capability of `sql-ai-tools` — *parse, type-check, and name-resolve CRDB
SQL without a cluster, with structured fix-suggestion errors* — is absent
from `claude-plugin`. The plugin's `validate-sql` hook is a regex deny-list;
it does not catch a misspelled column, a wrong-type INSERT, an unknown
function, or a CRDB-specific syntax error. Anything an agent does through
`claude-plugin` requires a live database round-trip.

**Is `claude-plugin` made obsolete by `sql-ai-tools`?** Also no.
`claude-plugin` covers things this repo is not trying to be: cluster
lifecycle via Cloud MCP, curated agent personas, distribution-ready skills,
and a real packaging/install story. The hooks and sub-agents are ergonomics
work this repo neither does nor should do.

**Genuine overlap is small but real.** `list_tables` / `describe_table` /
SQL execution exist in both. In a connected setup, an agent could perform
these via either backend. This is the one place to be deliberate about
positioning so users do not pick blindly.

## How they fit together

The two projects line up cleanly along orthogonal axes:

- **`claude-plugin`** is the *distribution and agent-knowledge layer*: how a
  CRDB user installs CRL tooling into Claude, and how Claude knows CRDB
  best practice.
- **`sql-ai-tools`** is the *offline validation engine*: the thing that
  turns "write SQL → run → guess fix → retry ×5" into "validate → fix → run
  ×1" without a cluster round-trip.

The most natural endgame is for `claude-plugin` to add `crdb-sql mcp` as a
third backend alongside `cockroachdb-toolbox` and `cockroachdb-cloud`. The
plugin already proxies two backends; a third is a `.mcp.json` change. A
single install would then give a Claude user offline validation, live
execution, and cloud control through one entry point.

## Risks if we do nothing

The real risk is **positioning**, not redundancy. Two CRL projects both
described as "CockroachDB SQL tooling for Claude" will confuse adopters
and invite the "why both?" question every time either is mentioned.

Mitigations:

- A clear one-liner in this repo's `README.md` positioning `sql-ai-tools`
  as the offline/parser-grade complement to `claude-plugin`.
- Reciprocal mention from `claude-plugin` once `sql-ai-tools` has a
  shippable MCP surface.
- A coordination conversation with the `claude-plugin` maintainers about
  adding `crdb-sql mcp` as an opt-in backend.

## Recommendation

Keep `sql-ai-tools`. It solves a problem `claude-plugin` structurally
cannot solve. Reposition the README to make the complementary relationship
explicit, and open a conversation with the `claude-plugin` maintainers
about adding `crdb-sql` as a third MCP backend once the validation tools
stabilize.
