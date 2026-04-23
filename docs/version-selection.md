# Version selection

CockroachDB releases follow `Year.Quarter.Patch` (e.g. `25.4.0`, `26.2.1`).
Parser behavior — what SQL is accepted, how the AST is shaped, which
builtins are known — can change between quarters. `crdb-sql` ships
one binary per supported quarter so users get full parsing fidelity
against the cluster they actually run.

## The lazy path: just install `crdb-sql`

Download the latest release, untar, put the directory on `$PATH`:

```sh
tar xzf crdb-sql_*_linux_amd64.tar.gz -C /usr/local/bin
crdb-sql version
```

That single binary is built against the latest cockroachdb-parser
quarter. It handles every CLI command, the MCP server, and everything
in between. If you don't pass `--target-version`, this is what runs.

## The fidelity path: install per-quarter siblings

When you need to validate SQL against an older CRDB cluster (and care
that the parser matches), install the matching backend alongside the
latest binary. Per-quarter sibling binaries follow the naming
convention `crdb-sql-vXXX` and live in the same directory as `crdb-sql`
(or anywhere on `$PATH`).

Use `--target-version` and the routing layer in the latest binary
takes care of the rest:

```sh
crdb-sql --target-version 26.2.0 validate -e "SELECT 1"
```

When the requested Year.Quarter differs from the latest binary's
quarter, `crdb-sql` locates the matching `crdb-sql-vXXX` sibling and
re-execs into it (via `syscall.Exec` on Unix; spawn-then-exit on
Windows). All flags, stdin/stdout/stderr, environment, and exit codes
pass through unchanged. The MCP `stdio` transport works because the
re-exec happens before any handshake.

## What ships today

The current release contains only the latest backend:

| CRDB cluster version | Binary       | Status         |
|----------------------|--------------|----------------|
| 26.2.x               | `crdb-sql`   | shipped        |

Older quarters are tracked as separate follow-on issues — see #122
for v26.1 — and will appear in the table above as they ship. Until
then, asking for an older `--target-version` produces a hard error
with an install hint (see "Missing-backend errors" below).

Patch differences are routing-irrelevant: `--target-version 26.2.0`
and `--target-version 26.2.7` both route to `crdb-sql-v262`. The patch
component is preserved verbatim in the response envelope so tooling
can still distinguish the cluster patch level.

## `crdb-sql versions`: discovery

To see what's actually installed:

```sh
$ crdb-sql versions
this binary  crdb-sql-v262    26.2        /usr/local/bin/crdb-sql
```

Once additional siblings are installed, they appear here too:

```sh
$ crdb-sql versions
this binary  crdb-sql-v262    26.2        /usr/local/bin/crdb-sql
sibling      crdb-sql-v261    26.1        /usr/local/bin/crdb-sql-v261
```

The discovery walks both the directory containing the running binary
and `$PATH`, deduplicates, and sorts newest-first. JSON output is
available via `-o json`.

## Missing-backend errors

If you ask for a quarter that isn't installed, `crdb-sql` exits
non-zero with a hint instead of silently falling back to the wrong
parser:

```
$ crdb-sql --target-version 25.1.0 validate -e "SELECT 1"
crdb-sql: --target-version 25.1.0 requires the crdb-sql-v251 backend,
which is not installed alongside this binary or in $PATH.
This binary is crdb-sql-v262.
Available backends:
  crdb-sql-v262    (this binary)
Install the v251 backend from the GitHub release, or omit --target-version
to use the latest parser.
```

Silent fallback is exactly the bug this routing layer exists to
prevent — running 25.1 SQL through a 26.2 parser can either
misparse or, worse, accept syntax that 25.1 will reject. The hard
error keeps that visible.

## Relationship to `--target-version` advisory warnings

`--target-version` predates this routing layer (see #82) and continues
to do its original job: stamping `target_version` in the response
envelope and emitting a mismatch warning when the parser version
disagrees with the targeted cluster. With per-quarter backends
installed, the routing closes the gap — the warning fires only when
the user asks for a quarter they did not install — and the user gets
the matching parser instead of just an advisory note.

## How to add support for a new quarter

When CockroachDB cuts a new quarterly release:

1. Tag the parser fork at the matching version (e.g. `v0.27.1`).
2. Generate the builtin-stubs file:
   ```sh
   make generate-builtins STUBS_VERSION=v27.1
   ```
3. Create `build/go.v271.mod` by copying `go.mod` and pointing the
   `replace` directive at the new fork tag.
4. Append a `builds:` entry to `.goreleaser.yaml` mirroring the
   existing one with `binary: crdb-sql-v271`, `gomod.modfile:
   build/go.v271.mod`, and ldflag stamp `builtQuarterStamp=v271`.
5. Update `QUARTERS` in the `Makefile` and the `quarters:` list in
   `build/parser-versions.yaml`.
6. If this quarter is the new latest, bump `LATEST_QUARTER` in the
   `Makefile` and the latest-binary's `builtQuarterStamp` in
   `.goreleaser.yaml` to match.

The CI matrix and size-cap script auto-pick up the new quarter on the
next release.
