---
name: commit-helper
description: Help create git commits and PRs with properly formatted messages following CockroachDB conventions. Use when committing changes or creating pull requests.
---

# CockroachDB Commit Helper

Help the user create properly formatted commit messages and release notes that follow CockroachDB conventions.

## Workflow

1. **Analyze the changes**: Run `git diff --staged` or `git diff` to understand what was modified
2. **Determine the package prefix**: Identify the primary package affected
3. **Ask the user** for:
   - Issue number if not already known
4. **Write the subject line**: Imperative mood, no period, under 72 characters
5. **Write the body**: Explain the before/after, why the change was needed
6. **Add issue references**: Include Resolves as appropriate
7. **Create the commit** using the properly formatted message

## Commit Message Structure

**Basic Format:**
```
package: imperative title without period

Detailed explanation of what changed, why it changed, and
how it impacts users. Explain the problem that existed
before and how this commit solves it.

Include context about alternate approaches considered and
any side effects or consequences.

Resolves: #123
```

**Key Requirements:**
- **Must** include issue
- **Must** separate subject from body with blank line
- **Recommended** prefix subject with affected package/area
- **Recommended** use imperative mood in subject (e.g. "fix bug" not "fixes bug")
- **Recommended** wrap body at 72-100 characters

## Issue References
- `Resolves: #123` - Auto-closes issue on PR merge
- `Informs: #123` - Is only a part of an issue and doesn't close it.
- `See also: #456, #789` - Cross-references issues

## How to Avoid Common Pitfalls
- Write specific descriptions that explain the impact to users
- End subject lines without punctuation
- Explain the "why" behind changes, not just the "what"

## Pull Request Guidelines

**Default: one commit per PR.** Squash WIP, "address review", and other
incremental commits before opening or updating the PR. The PR title and
body should mirror the single commit's title and body.

**Multi-commit PRs are allowed only when each commit is independently
meaningful and stands on its own.** Typical valid shapes:
- A pure refactor commit followed by a feature commit that depends on it
- Two or more unrelated mechanical changes grouped for reviewer convenience

If commits don't stand on their own (e.g. one fixes a bug introduced by
another, or one is a partial step toward the next), squash them. When in
doubt, prefer one commit.

For multi-commit PRs, the PR body must summarize the end goal and
describe each commit so the reviewer can read commit-by-commit. When the
commits are unrelated mechanical changes, it's fine to say the
individual commits speak for themselves.

## Applying Review Feedback

**Always amend the relevant existing commit(s) in the PR rather than
adding fixup-style follow-up commits.** Use `git commit --amend` for the
tip commit, or `git rebase -i` to edit earlier commits, then force-push
with `--force-with-lease`.

Do **not** create commits like "address review feedback", "fix bug in
previous commit", or "retry approach from prior commit". They clutter
`git log`, make bisect harder, and obscure the final intent of each
change. The end state of the PR branch should look as if the feedback
had been incorporated from the start.

Exception: if the reviewer explicitly asks for a separate commit (for
example, to make a specific change easy to revert), follow their
request.
