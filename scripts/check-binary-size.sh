#!/usr/bin/env bash
# Enforce the release binary size cap. Invoked by .github/workflows/release.yml
# after goreleaser builds, but also runnable locally after
# `goreleaser release --snapshot --clean --skip=publish`.
#
# Fails (non-zero exit + ::error annotation) when:
#   - dist/ is missing entirely (goreleaser never ran)
#   - no crdb-sql binaries are found under dist/<target>/
#   - any binary is zero bytes (build produced empty output)
#   - any binary exceeds MAX_BYTES (default 50 MiB)
#
# Each failure mode emits a distinct message so CI logs point at the real
# cause instead of a generic "size check failed".
#
# Usage:
#   scripts/check-binary-size.sh                 # 50 MiB cap, dist/ in CWD
#   MAX_BYTES=$((30*1024*1024)) scripts/check-binary-size.sh
set -euo pipefail

MAX_BYTES="${MAX_BYTES:-$((50 * 1024 * 1024))}"

# Reject non-numeric MAX_BYTES up front. Without this guard, a typo or unit
# suffix (e.g. MAX_BYTES=50M) makes the `-gt` test below fail with exit 2
# inside an `if`, which `set -e` does NOT catch — the size cap silently
# becomes a no-op.
if ! [[ "$MAX_BYTES" =~ ^[0-9]+$ ]]; then
  echo "::error::MAX_BYTES must be a positive integer (got: $MAX_BYTES)"
  exit 2
fi

if [ ! -d dist ]; then
  echo "::error::dist/ directory missing — goreleaser did not run"
  exit 1
fi

# goreleaser lays out per-target dirs like dist/crdb-sql_linux_amd64_v1/.
# Use find + mapfile so an empty match is detectable up front (a bash glob
# would expand literally and downstream commands would fail with a less
# actionable message). `-L` follows symlinks so a symlinked dist/ or a
# symlinked binary is still counted instead of silently skipped.
#
# Run find through temp files so we can inspect both its exit code AND its
# stderr. Process substitution `<(...)` would discard `find`'s exit code,
# and `set -o pipefail` does not apply across it. Without checking BOTH,
# an unreadable target dir (perm flip, symlink loop, transient I/O error)
# could let `find` exit 1 with a partial stdout list — sometimes with
# stderr output, sometimes without (e.g. `-L` symlink-loop behavior is
# platform-dependent) — and `mapfile` would happily read whatever made it
# through. The script would then size-check a SUBSET of the binaries with
# no warning, silently bypassing the cap for any binary in the affected
# target.
#
# Both mktemp calls are guarded so a failure produces a structured
# annotation rather than the raw `mktemp:` stderr that `set -e` would
# otherwise dump. Trap covers EXIT/INT/TERM so Ctrl-C in CI doesn't leak
# temp files. Single registration — if you add another temp file, append
# to this trap rather than re-registering.
find_errs=$(mktemp) || { echo "::error::failed to create temp file for find stderr"; exit 1; }
find_out=$(mktemp) || { echo "::error::failed to create temp file for find output"; rm -f "$find_errs"; exit 1; }
trap 'rm -f "$find_errs" "$find_out"' EXIT INT TERM

find_rc=0
find -L dist -mindepth 2 -maxdepth 2 -type f -name crdb-sql >"$find_out" 2>"$find_errs" || find_rc=$?
if [ "$find_rc" -ne 0 ]; then
  echo "::error::find failed (exit $find_rc) while enumerating dist/:"
  cat "$find_errs"
  exit 1
fi
if [ -s "$find_errs" ]; then
  echo "::error::find emitted errors while enumerating dist/:"
  cat "$find_errs"
  exit 1
fi
mapfile -t bins < "$find_out"
if [ "${#bins[@]}" -eq 0 ]; then
  echo "::error::no crdb-sql binaries found under dist/*/ — goreleaser layout changed or build produced nothing"
  exit 1
fi

fail=0
for f in "${bins[@]}"; do
  # `wc -c < file` is POSIX and works on both GNU coreutils (Linux CI) and
  # BSD (macOS local runs); `stat -c%s` is GNU-only.
  # Guard the redirect: if $f became unreadable between find and wc (race
  # with a parallel `--clean`, perm flip), the bare redirect would let
  # set -e abort with a useless `integer expression expected` and no file
  # context. Catch the failure and emit a structured annotation instead.
  if ! sz=$(wc -c < "$f" 2>/dev/null); then
    echo "::error file=$f::could not read binary"
    fail=1
    continue
  fi
  sz=$(printf '%s' "$sz" | tr -d '[:space:]')
  printf '%s  %d bytes\n' "$f" "$sz"
  if [ "$sz" -eq 0 ]; then
    echo "::error file=$f::zero-byte binary — build produced empty output"
    fail=1
  elif [ "$sz" -gt "$MAX_BYTES" ]; then
    echo "::error file=$f::binary $sz bytes exceeds $MAX_BYTES bytes cap"
    fail=1
  fi
done
exit "$fail"
