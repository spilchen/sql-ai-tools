---
name: next
description: Show the next GitHub issue to work on, prioritized by wave and tier, excluding issues that already have a worktree or open PR.
---

# Next Work Item

Show the user which GitHub issue to pick up next. Gathers open issues,
filters out anything already in flight (has a worktree or open PR), sorts
by priority, and recommends a starting point.

---

## Step 1: Gather Data

Run these three commands **in parallel** (they are independent):

```bash
gh issue list --state open --json number,title,labels,body --limit 100
```

```bash
git worktree list --porcelain
```

```bash
gh pr list --state open --json number,title,headRefName --limit 50
```

---

## Step 2: Build the In-Flight Set

Extract issue numbers from **worktree branch names** and **PR branch
names**. Both use the same convention: the branch ref contains
`gh-<number>` (e.g. `refs/heads/spilchen/gh-31/260420/1029a/releaser`).

For worktrees, scan lines that start with `branch ` in the porcelain
output and apply the regex `gh-(\d+)` to extract the issue number.

For PRs, apply the same regex to each PR's `headRefName` field.

Build a map of in-flight issue numbers. For each, record whether it has
a worktree, an open PR, or both — this is used for the annotation in
Step 5.

---

## Step 3: Parse Issue Labels

For each open issue, extract these fields from the `labels` array (each
label is an object with a `name` field):

| Field | Source label pattern | Default if missing |
|-------|---------------------|--------------------|
| wave  | `wave-N` → integer N | 999 (sorts last)  |
| tier  | `tier-N` → integer N | 999 (sorts last)  |
| surface | `cli`, `mcp` | _(empty)_ |
| phase | `harness`, `stretch`, `release` | _(empty)_ |

---

## Step 4: Partition and Sort

Split issues into two lists:

- **Available** — issue number is NOT in the in-flight set.
- **In-flight** — issue number IS in the in-flight set.

Sort each list by:
1. Wave ascending (lower wave = higher priority)
2. Tier ascending (lower tier = higher priority)
3. Issue number ascending (lower = filed earlier)

---

## Step 5: Present Results

### Status line

Print a one-line summary:

```
N open issues, M in flight, K available
```

### Available issues table

Group available issues by wave. For each wave that has available issues,
print a section header and a markdown table. Use `Unassigned` for issues
with no wave label (wave=999).

```markdown
### Wave 1
| Issue | Title | Tier | Labels |
|-------|-------|------|--------|
| #5    | Add crdb-sql format subcommand | tier-1 | cli, wave-1 |
| #6    | Add crdb-sql validate | tier-1 | cli, wave-1 |
| #11   | Add schema loader for CREATE TABLE files | tier-2 | cli, wave-1 |
```

In the **Labels** column, list all labels except the tier label (since
tier has its own column). If tier is missing, show `—` in the Tier
column.

If there are no available issues, say so and skip to the in-flight
table.

### In-flight issues

Show in-flight issues in a collapsed block:

```markdown
<details>
<summary>M in-flight issues</summary>

| Issue | Title | Status |
|-------|-------|--------|
| #3    | Add crdb-sql parse | worktree + PR |
| #30   | Add fidelity test suite | worktree |

</details>
```

The **Status** column shows `worktree`, `PR`, or `worktree + PR`.

---

## Step 6: Recommend

Pick the first issue from the sorted available list as the recommendation.
Print it after the tables:

```markdown
**Recommendation:** #N (short title) — rationale.
```

Build the rationale from the issue's labels:
- wave-0 → "foundation — must land before downstream issues"
- wave-1 + tier-1 → "foundational command, all wave-0 work resolved"
- `harness` → "scaffolding that other features build on"
- Otherwise → "lowest-wave, lowest-tier available issue"

If the issue has a `Depends on:` line in its body that references
in-flight issues, note that: "depends on in-flight #X — consider
picking up #X first or working in parallel."
