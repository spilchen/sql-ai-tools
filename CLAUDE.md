# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Coding Guidelines

### Engineering Standards

Key concepts and abstractions should be explained clearly, and lifecycles and
ownership clearly stated. Whenever possible, you should use examples to make the
code accessible to the reader. Comments should always add depth to the code
(rather than repeating the code).

When reviewing, other than technical correctness, you should also focus on the
above aspects. Do not over-emphasize on grammar and comment typos, prefix with
"nit:" in reviews.

### Resources

- **Design Documents**: `/docs/`

### When generating PRs and commit records

Use the `commit-helper` skill (invoked via `/commit-helper`) when creating commits and PRs.

- For multi-commit PRs, summarize each commit in the PR record.
- Do not include a test plan unless explicitly asked by the user.

# Interaction Style

* Be direct and honest.
* Skip unnecessary acknowledgments.
* Correct me when I'm wrong and explain why.
* Suggest better alternatives if my ideas can be improved.
* Focus on accuracy and efficiency.
* Challenge my assumptions when needed.
* Prioritize quality information and directness.
