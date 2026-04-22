---
name: dev-loop
description: Automate the post-implementation dev cycle — build, review, fix, open PR, rebase, and merge. Invoke after planning and implementation are complete.
---

# Dev Loop

Automate the post-implementation pipeline: build verification, commit,
review/fix loop, PR creation, rebase, and merge. Designed for
single-commit PRs that follow CockroachDB conventions.

Invoke with no arguments after you have finished implementing a feature:

```
/dev-loop
```

---

## Step 0: Pre-flight

### 0a. Verify there are changes to ship

```bash
git status --porcelain
```

If the output is empty, abort: `No changes detected. Nothing to ship.`

### 0b. Detect issue number from branch name

```bash
git rev-parse --abbrev-ref HEAD
```

Apply the regex `gh-(\d+)` to the branch name. If it matches, store the
captured group as `issue_number`. Otherwise set `issue_number = ""`.

Save the full branch name as `branch`.

---

## Step 1: Build Verification

Track attempts: `build_attempt = 1`, `max_build_attempts = 3`.

### 1a. Format, lint, and test

Run these sequentially — each depends on the previous:

```bash
make fmt
```

```bash
make lint
```

```bash
make test
```

```bash
make test-integration
```

`make test-integration` runs the build-tagged integration suite. The
tests `t.Skip` cleanly when no cockroach binary is reachable, so this
step is safe to run on a machine that has not installed cockroach. Set
`COCKROACH_BIN=/path/to/cockroach` (or `CRDB_TEST_DSN=postgres://...`)
to actually exercise the integration suite — these tests are
intentionally not in CI, so the dev-loop is the place that catches
regressions in cluster-touching code.

### 1b. Handle failures

If any command fails:

1. Read the error output and fix the issue in the source code.
2. Increment `build_attempt`.
3. If `build_attempt > max_build_attempts`, abort:
   `Build failed after 3 attempts. Last error: [error]. Changes are
   uncommitted — fix manually and re-run /dev-loop.`
4. Otherwise, go back to Step 1a.

Once all three pass, proceed.

---

## Step 2: Commit

Invoke the `commit-helper` skill via the Skill tool. It will analyze the
changes, ask the user for any missing context (e.g., issue number if one
cannot be detected from the branch name), and create a single commit
following CockroachDB conventions.

If `issue_number` was detected in Step 0b, mention it when invoking the
skill so it can include the appropriate `Resolves:` or `Informs:` line.

---

## Step 3: Pre-PR Review Loop

Run a local review cycle before the PR is visible to anyone. This catches
issues early and avoids noisy "changes requested" reviews on a fresh PR.

Set `review_iteration = 1`, `max_review_iterations = 3`.

### 3a. Run review

Invoke the `pr-review-toolkit:review-pr` skill via the Skill tool with no
extra arguments. No PR exists yet — the skill reviews the current
branch's diff against main (the local committed changes).

Capture the full findings.

If the skill errors or returns no findings, do NOT treat the PR as
"clean". Abort: `Review skill failed. Not proceeding without a review.`

### 3b. Classify findings

Walk through every finding and bucket into severities:

| Source label | Normalized severity |
|---|---|
| `critical`, `CRITICAL`, `must fix` | `critical` |
| `important`, `HIGH`, `should fix` | `important` |
| `suggestion`, `MEDIUM`, `LOW`, `nice to have`, `nit` | `suggestion` |

Compute `critical_count`, `important_count`, `suggestion_count`.

```
clean = (critical_count == 0 AND important_count == 0)
```

### 3c. Decision

**If `clean == true`:**
- Log: `Pre-PR review clean (iteration <review_iteration>). <suggestion_count> suggestion(s) noted.`
- Proceed to Step 4.

**If `clean == false` AND `review_iteration < max_review_iterations`:**
- Log: `Pre-PR review: <critical_count> critical, <important_count> important (iteration <review_iteration>/<max_review_iterations>). Fixing...`
- Fix every critical and important issue in the source code. Suggestions
  are optional — fix only if trivial.
- Re-run build verification (Step 1a-1b, resetting `build_attempt = 1`).
- Stage only the files that were modified by the fixes, then amend:
  ```bash
  git add <changed files>
  git commit --amend --no-edit
  ```
- Increment `review_iteration`.
- Go back to Step 3a.

**If `clean == false` AND `review_iteration >= max_review_iterations`:**
- Log the remaining issues.
- Ask the user:
  `Pre-PR review loop hit its limit (3 iterations) with unresolved
  issues: <critical_count> critical, <important_count> important.
  **open** the PR anyway, or **abort**?`
- If the user says **abort**: stop. Print remaining issues and the branch
  name so the user can fix manually and re-run `/dev-loop`.
- If the user says **open**: proceed to Step 4.

---

## Step 4: Open PR

### 4a. Push the branch

```bash
git push -u origin HEAD
```

If push fails because the remote branch already exists and has diverged
(e.g., from a previous aborted dev-loop run), retry with:

```bash
git push --force-with-lease
```

If push fails for any other reason, report the error and abort.

### 4b. Create the PR

Invoke the `commit-helper` skill via the Skill tool to create the PR.
The commit already follows CockroachDB conventions (created in Step 2),
so the skill will mirror the commit subject/body into the PR title/body.

Capture the PR number and URL from the output.

### 4c. Report

Print: `PR #<number> opened: <url>`

---

## Step 5: Rebase and Merge Loop

Set `merge_iteration = 1`, `max_merge_iterations = 3`.

### 5a. Rebase onto main

```bash
git fetch origin main
git rebase origin/main
```

**If rebase has conflicts:**

Attempt automatic resolution for trivial conflicts (e.g., both sides
added entries to the same list). If conflicts cannot be resolved:

```bash
git rebase --abort
```

Tell the user:
`Rebase has conflicts I cannot resolve. PR is open at <url>. Resolve
manually and run /auto-merge-pr <pr_number>.`

Stop here.

**If rebase succeeds:**

```bash
git push --force-with-lease
```

### 5b. Build verification after rebase

Rebase can introduce breakage from upstream changes. Reset
`build_attempt = 1` and re-run the full build sequence:

```bash
make fmt
```

```bash
make lint
```

```bash
make test
```

```bash
make test-integration
```

(Same notes as Step 1a apply: integration tests skip cleanly without
`COCKROACH_BIN` / `CRDB_TEST_DSN`.)

If `make fmt` modified any files, stage and amend before proceeding:

```bash
git add <changed files>
git commit --amend --no-edit
git push --force-with-lease
```

If any command fails, fix the issue, amend the commit, force-push, and
retry (up to `max_build_attempts = 3`). If exhausted, tell the user and
stop.

### 5c. Run review

Invoke the `pr-review-toolkit:review-pr` skill via the Skill tool.

Classify findings using the same severity buckets from Step 3b.

### 5d. Decision

Determine whether this is a self-PR (needed by both the clean and
exhausted-iterations paths below):

```bash
pr_author=$(gh pr view <pr_number> --json author --jq '.author.login')
viewer=$(gh api user --jq .login)
```

Set `is_self = (pr_author == viewer)`.

**If `clean == true`:**

Build the review body (same format as `auto-merge-pr` Step 5):

```markdown
## Automated PR Review

**Verdict:** <verdict string — see below>

### Summary
- Critical: 0
- Important: 0
- Suggestions: <suggestion_count>

<details>
<summary><suggestion_count> suggestions</summary>

- [<agent>] <file>:<line> — <short description>

</details>

_Generated by the `/dev-loop` skill._
```

Pick the review verb and verdict:

- `is_self == false`: `--approve`, verdict `Approved & queued for merge`
- `is_self == true`: `--comment`, verdict `Approved & queued for merge (posted as comment — self-PR)`

Post the review:

```bash
gh pr review <pr_number> <--approve|--comment> --body "$(cat <<'EOF'
<review body>
EOF
)"
```

Queue the merge:

```bash
gh pr merge <pr_number> --auto --rebase
```

If `gh pr merge --auto` fails because auto-merge is disabled, report the
failure but do NOT attempt a non-auto merge.

Proceed to Step 6.

**If `clean == false` AND `merge_iteration < max_merge_iterations`:**

- Log: `Post-PR review: <critical_count> critical, <important_count> important (iteration <merge_iteration>/<max_merge_iterations>). Fixing...`
- Fix every critical and important issue.
- Re-run build verification (Step 5b, resetting `build_attempt = 1`).
- Stage only the files modified by fixes, amend, and force-push:
  ```bash
  git add <changed files>
  git commit --amend --no-edit
  git push --force-with-lease
  ```
- Increment `merge_iteration`.
- Go back to Step 5a (rebase again — main may have moved).

**If `clean == false` AND `merge_iteration >= max_merge_iterations`:**

Build a review body listing all remaining issues:

```markdown
## Automated PR Review

**Verdict:** <verdict string — see below>

### Summary
- Critical: <critical_count>
- Important: <important_count>
- Suggestions: <suggestion_count>

### Critical Issues
- [<agent>] <file>:<line> — <short description>

### Important Issues
- [<agent>] <file>:<line> — <short description>

<details>
<summary><suggestion_count> suggestions</summary>

- [<agent>] <file>:<line> — <short description>

</details>

_Generated by the `/dev-loop` skill._
```

Omit any section whose count is zero. Pick the review verb and verdict:

- `is_self == false`: `--request-changes`, verdict `Changes requested`
- `is_self == true`: `--comment`, verdict `Changes requested (posted as comment — GitHub blocks request-changes on own PR)`

Post the review, then tell the user:
`Merge review loop hit its limit (3 iterations). PR is at <url> with
<critical_count> critical, <important_count> important issues remaining.
Fix manually and run /auto-merge-pr <pr_number>.`

Stop here.

---

## Step 6: Final Report

Print a summary:

```markdown
## Dev Loop Complete

| Field | Value |
|-------|-------|
| Issue | #<issue_number> (or "—" if none) |
| PR | #<pr_number> — <url> |
| Branch | <branch> |
| Verdict | Approved & queued for merge |
| Pre-PR review iterations | <N> |
| Post-PR review iterations | <M> |
| Suggestions noted | <suggestion_count> (not blocking) |

Auto-merge is queued. The PR will merge once CI checks pass.
```

---

## Abort Protocol

At any point where the skill aborts, follow this protocol:

1. Print a clear error message explaining what failed and why.
2. Print the current state:
   - Branch name
   - PR URL (if one was opened)
   - What completed vs. what remains
3. Do NOT delete the branch, worktree, or PR.
4. Suggest the manual recovery path (e.g., "fix the issue and re-run
   `/dev-loop`" or "run `/auto-merge-pr <N>`").

---

## Constraints

- **Single commit per PR.** Never create multiple commits. Always amend.
  This intentionally overrides the default "always create NEW commits"
  policy for the duration of the dev-loop execution.
- **Max 3 review-fix iterations** per loop (pre-PR and post-PR).
- **Max 3 build-fix attempts** per build verification pass.
- **Never force-push without `--force-with-lease`.**
- **Never skip `make lint` or `make test`.**
- **Suggestions are informational.** Only critical and important findings
  block progress.
