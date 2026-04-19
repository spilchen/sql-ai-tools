---
name: issue-create
description: Use this skill when creating GitHub issues for this repo. Codifies the body template, label schema, and dependency-graph workflow (Depends on / Blocks lines + scripts/issue-graph.sh).
---

# GitHub Issue Creation

Use this skill whenever the user asks to create, file, or open a GitHub
issue for this repo (`spilchen/sql-ai-tools`). It encodes the body
template, label schema, and dependency-graph workflow that the existing
issues already follow.

## Issue body template

Every issue body uses this structure:

```
<one-line scope summary>

**Scope**
- bullet list of what's in scope
- be explicit about what's NOT in scope when ambiguity is likely

**Demo**
`<command>` produces <observable end-to-end result>.

**Files**
- `path/to/file`
- `another/path`

## Dependencies
- **Depends on:** #X, #Y     (or `none`)
- **Blocks:** #A, #B          (or `none`)
```

The **Demo** line is required — every issue must deliver an end-to-end
visible change (a CLI command, an MCP tool response, an envelope field).
Internal-only refactors are not standalone issues; fold them into the
feature issue that needs them.

## Label schema

Pick from the labels already in the repo. Don't invent new ones without
asking.

| Group | Labels | Pick when |
|-------|--------|-----------|
| Surface | `cli`, `mcp` | Pick at least one if the change is user-facing. Both is fine when the same capability lands in both surfaces. |
| Tier | `tier-1`, `tier-2`, `tier-3` | Tier 1 = zero-config offline, Tier 2 = schema-aware offline, Tier 3 = cluster-connected. Skip if the issue is pure scaffolding. |
| Phase modifier | `harness`, `stretch`, `release` | `harness` for foundational scaffolding; `stretch` for differentiator/post-MVP work; `release` for packaging and docs. |

**Never apply `wave-N` labels manually** — they are derived from the
dependency graph by `scripts/issue-graph.sh`. Hand-applied wave labels
will be overwritten.

## Workflow

1. **Title** — concise, imperative, no trailing period
   (e.g. "Add crdb-sql validate", "Replace placeholder main.go with
   cobra CLI skeleton").

2. **Draft the body** — fill in the template above. The Scope/Demo/Files
   triple is what reviewers and future-Claude will scan.

3. **Determine dependencies** — `gh issue list --state open --limit 100`
   to see what exists, then list the upstream issue numbers under
   `Depends on:`. Use `none` if the issue stands alone. Use real `#N`
   refs only — vague text like "most feature issues" won't be parsed by
   the graph script.

4. **Create the issue** — use the gh heredoc pattern so the body keeps
   its formatting and any backticks/quotes are preserved verbatim:
   ```bash
   gh issue create \
     --title "Add crdb-sql foo subcommand" \
     --label "tier-1,cli" \
     --body "$(cat <<'EOF'
   <body content here>
   EOF
   )"
   ```

5. **Update back-pointers** — for every issue listed under
   `Depends on:`, edit its body and add the new issue number to its
   `Blocks:` line. The script doesn't compute Blocks, so they rot if you
   skip this step.

6. **Refresh wave labels** — run `scripts/issue-graph.sh`. The script
   reads the `Depends on:` edges, topo-sorts, and re-applies `wave-N`
   labels to every open issue. It auto-creates new `wave-N` labels when
   the graph deepens.

## Closing or editing an dependency

After closing an issue or editing any `Depends on:` line, re-run
`scripts/issue-graph.sh`. Closed deps are treated as resolved, so
downstream issues drop a wave automatically.

## Notes

- Don't include a `Wave:` line in the body. Wave is a label, not body
  text — keeping it out of the body prevents it from rotting.
- The `Blocks:` line is human-maintained documentation. It's redundant
  with the graph (the script could derive it) but useful when reading
  one issue in isolation.
- Use `gh repo view --json nameWithOwner` to confirm the right repo
  before creating, especially when working in a directory that's not the
  default repo for the gh context.
