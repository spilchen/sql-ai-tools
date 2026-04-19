#!/usr/bin/env bash
# WorktreeRemove hook for Claude Code's `--worktree` feature.
#
# Best-effort cleanup. Claude Code ignores this hook's exit code, so we
# never block removal — we only surface warnings the user might miss in a
# busy session and prune stale `.git/worktrees/` refs.
#
# We intentionally do NOT use `set -e`: each check below is independent,
# and a single failure shouldn't suppress the others. Failures are
# announced explicitly to stderr instead.
set -uo pipefail

payload=$(cat)

# `jq -er` returns non-zero on null/missing. If we can't even parse the
# payload there's no point continuing — print a clear error so the
# user sees it in `claude --debug` output and exit 0 (don't pretend to
# block removal; Claude Code ignores our exit code anyway).
if ! worktree_path=$(printf '%s' "$payload" | jq -er '.worktree_path'); then
	echo "wt-remove: payload missing .worktree_path" >&2
	exit 0
fi
if ! source_path=$(printf '%s' "$payload" | jq -er '.source_path'); then
	echo "wt-remove: payload missing .source_path" >&2
	exit 0
fi
# .branch may legitimately be absent (detached worktrees).
branch=$(printf '%s' "$payload" | jq -r '.branch // ""')

if [ -d "$worktree_path" ]; then
	if dirty=$(git -C "$worktree_path" status --porcelain 2>&1); then
		if [ -n "$dirty" ]; then
			printf 'wt-remove: %s has uncommitted changes (Claude Code is removing it anyway)\n' "$worktree_path" >&2
		fi
	else
		printf 'wt-remove: could not run git status in %s: %s\n' "$worktree_path" "$dirty" >&2
	fi

	# Branch not pushed anywhere: caller may lose work after removal.
	if [ -n "$branch" ]; then
		if ! git -C "$worktree_path" rev-parse --verify --quiet "@{upstream}" >/dev/null 2>&1; then
			printf 'wt-remove: branch %s has no upstream (unpushed commits will be unreachable)\n' "$branch" >&2
		fi
	fi
fi

# Let stderr through — a failure here usually means a corrupt
# `.git/worktrees/<name>` entry that needs human attention; silencing it
# is exactly the bug we don't want.
git -C "$source_path" worktree prune >/dev/null
