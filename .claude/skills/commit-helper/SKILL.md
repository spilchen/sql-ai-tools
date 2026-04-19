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
8. **Create the commit** using the properly formatted message

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
- **Single-commit PRs**: PR title should match commit title, PR body should match commit body
- **Multi-commit PRs**: The body should summarize the end goal that the set of commits achieves and give the reader the context necessary to review the PR commit by commit (for example, the first commits might get refactors out of the way so that the last commit can hook everything up). When there isn't an overarching connection between the commits (maybe the PR groups a few mechanical changes that are not related) it is fine to say that the individual commits speak for themselves.
