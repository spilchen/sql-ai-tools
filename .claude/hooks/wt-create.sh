#!/usr/bin/env bash
# WorktreeCreate hook for Claude Code's `--worktree` feature.
#
# Replaces Claude's default `git worktree add` so the new branch follows
# the issue-aware naming convention from CLAUDE.md (Worktree Workflow).
# The hook contract: read the event JSON from stdin, return the worktree
# path on stdout, exit non-zero to abort creation.
#
# Branch name source of truth:
#   - SQLAI_WT_BRANCH env var (set by scripts/wt-new), if present.
#   - Fallback `wt/<slug>` for direct `claude --worktree <slug>` calls,
#     so the feature still works without our wrapper.
set -euo pipefail

payload=$(cat)

# `jq -er` exits non-zero on null/missing fields, which then aborts the
# hook (and the worktree create) instead of letting the literal string
# "null" be passed to `git worktree add` later.
worktree_path=$(printf '%s' "$payload" | jq -er '.worktree_path')
source_path=$(printf '%s' "$payload" | jq -er '.source_path')
slug=$(basename "$worktree_path")

branch=${SQLAI_WT_BRANCH:-wt/$slug}

# `origin/HEAD` is a local symbolic ref pointing at whatever the remote's
# default branch was at the last `git remote set-head` (or the last
# clone). No `git fetch` runs here, so this is *not* guaranteed fresh —
# the caller is responsible for `git fetch` if they care. We only fall
# back to local HEAD when origin/HEAD has never been resolved (e.g.
# brand-new clone with no remote).
base=origin/HEAD
if ! git -C "$source_path" rev-parse --verify --quiet "$base" >/dev/null; then
	printf 'wt-create: origin/HEAD not set; using local HEAD as base\n' >&2
	base=HEAD
fi

# --no-track keeps the new branch from inheriting `origin/main` as its
# upstream. The first `git push` will need `-u origin <branch>` (or rely
# on push.autoSetupRemote in Git 2.37+) and will create a matching
# remote branch — which is exactly what the PR flow wants.
git -C "$source_path" worktree add --no-track -b "$branch" "$worktree_path" "$base" >&2

printf '%s\n' "$worktree_path"
