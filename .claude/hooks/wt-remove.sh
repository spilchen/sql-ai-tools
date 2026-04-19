#!/usr/bin/env bash
# WorktreeRemove hook for Claude Code's `--worktree` feature.
#
# Best-effort cleanup. Claude Code ignores this hook's exit code, so we
# never block removal — we only surface warnings the user might miss in
# a busy session and prune stale `.git/worktrees/` refs.
#
# Hook contract (observed v2.1.114):
#   stdin JSON: { "session_id", "transcript_path", "cwd",
#                 "hook_event_name": "WorktreeRemove", "name": "<slug>" }
#   Earlier Anthropic-documented payloads used ".source_path" /
#   ".worktree_path" / ".branch"; we still read those if the v2.1.114
#   fields are missing, so the hook keeps working across version drift.
#   exit code is ignored by Claude Code; we only emit warnings to stderr.
#
# We intentionally do NOT use `set -e`: each check below is independent,
# and a single failure shouldn't suppress the others. Failures are
# announced explicitly to stderr instead.
set -uo pipefail

payload=$(cat)

# Field extraction. Primary keys (`.cwd`, `.name`) come from the v2.1.114
# payload; the `// .source_path`, `// empty` fallbacks pick up the older
# documented shape so we never silently no-op when the contract drifts.
source_path=$(printf '%s' "$payload" | jq -r '.cwd // .source_path // empty')
slug=$(printf '%s' "$payload" | jq -r '.name // empty')
worktree_path=$(printf '%s' "$payload" | jq -r '.worktree_path // empty')

if [ -z "$worktree_path" ] && [ -n "$source_path" ] && [ -n "$slug" ]; then
	worktree_path="$source_path/.claude/worktrees/$slug"
fi
if [ -z "$source_path" ] || [ -z "$worktree_path" ]; then
	echo "wt-remove: payload missing required fields; skipping" >&2
	exit 0
fi
# `branch` is optional — newer payloads may omit it for a detached worktree.
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

# Only stdout is silenced; stderr is intentionally passed through because
# a failure here usually means a corrupt `.git/worktrees/<name>` entry
# that needs human attention. Without `set -e`, an unchecked non-zero
# exit would be silently dropped, so capture it explicitly. (We can't
# use `if ! cmd; then printf "$?"`: the `!` operator turns the pipeline
# into its negated value, and that becomes the new `$?` — so the warning
# would always print "exit 0".)
git -C "$source_path" worktree prune >/dev/null
prune_exit=$?
if [ "$prune_exit" -ne 0 ]; then
	printf 'wt-remove: git worktree prune failed (exit %d) in %s\n' "$prune_exit" "$source_path" >&2
fi
