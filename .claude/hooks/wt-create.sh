#!/usr/bin/env bash
# WorktreeCreate hook for Claude Code's `--worktree` feature.
#
# Replaces Claude Code's default `git worktree add` so the new branch
# follows the issue-aware naming convention from CLAUDE.md (Worktree
# Workflow).
#
# Hook contract (observed from Claude Code v2.1.114):
#   stdin JSON: { "session_id", "transcript_path", "cwd",
#                 "hook_event_name": "WorktreeCreate", "name": "<slug>" }
#   stdout: the absolute worktree path, one line, nothing else.
#   exit:   non-zero to abort worktree creation.
#
# Branch name source of truth:
#   - SQLAI_WT_BRANCH env var (set by scripts/wt-new), if present.
#   - Fallback `wt/<slug>` for direct `claude --worktree <slug>` calls,
#     so the feature still works without our wrapper.
set -euo pipefail

payload=$(cat)

# `jq -er` exits non-zero on null/missing fields, which aborts the hook
# (and worktree creation) instead of letting empty strings sneak through
# into a malformed `git worktree add`.
cwd=$(printf '%s' "$payload" | jq -er '.cwd')
slug=$(printf '%s' "$payload" | jq -er '.name')

# `cwd` is wherever the user invoked `claude --worktree` from — possibly
# a sub-directory. Anchor the worktree path to the repo root so the
# layout is identical no matter where the command was launched from.
repo_root=$(git -C "$cwd" rev-parse --show-toplevel)
worktree_path="$repo_root/.claude/worktrees/$slug"
branch=${SQLAI_WT_BRANCH:-wt/$slug}

# `origin/HEAD` is a local symbolic ref pointing at whatever the remote's
# default branch was at the last `git remote set-head` (or the last
# clone). No `git fetch` runs here, so this is *not* guaranteed fresh —
# the caller is responsible for `git fetch` if they care. Fall back to
# local HEAD only when origin/HEAD has never been resolved — common
# triggers: clone with `--no-checkout`, mirror clones, manually unset
# HEAD via `git remote set-head origin -d`, or no remote at all — AND
# HEAD is on a real branch. Branching off a detached HEAD would silently
# pick up an arbitrary commit, so we refuse that case loudly.
base=origin/HEAD
if ! git -C "$repo_root" rev-parse --verify --quiet "$base" >/dev/null; then
	if ! git -C "$repo_root" symbolic-ref -q HEAD >/dev/null; then
		printf 'wt-create: origin/HEAD not set and local HEAD is detached; refusing to branch off an arbitrary commit\n' >&2
		exit 1
	fi
	printf 'wt-create: origin/HEAD not set; using local HEAD as base\n' >&2
	base=HEAD
fi

# --no-track keeps the new branch from inheriting `origin/main` as its
# upstream. The first `git push` will need `-u origin <branch>` (or rely
# on push.autoSetupRemote in Git 2.37+) and will create a matching
# remote branch — which is exactly what the PR flow wants.
#
# Send git's stdout+stderr to our stderr so nothing pollutes our own
# stdout (Claude Code parses our stdout as the worktree path).
git -C "$repo_root" worktree add --no-track -b "$branch" "$worktree_path" "$base" >&2

printf '%s\n' "$worktree_path"
