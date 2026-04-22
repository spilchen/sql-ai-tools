// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package conn

import (
	"fmt"
	"strings"
)

// DDLOperation is one operation in a CockroachDB declarative schema
// changer plan, as rendered by `EXPLAIN (DDL, SHAPE)`. Op is the
// operation description with the leading tree connector stripped (e.g.
// "execute 4 system table mutations transactions", "backfill using
// primary index users_pkey- in relation users"). Targets holds the
// indented sub-lines that follow the operation, describing the index or
// constraint the operation acts on (e.g. "into users_pkey+ (id; name)",
// "from users@[5] into users_pkey+"). Most operations have no targets;
// the slice is nil in that case.
//
// CRDB conventions in these strings: a trailing `-` on an index name
// (e.g. `users_pkey-`) marks the version being torn down; a trailing
// `+` marks the version being built up. `t@[N]` references a table's
// internal index by ID. These are passed through verbatim because they
// are part of CRDB's diagnostic vocabulary.
//
// SHAPE output presents operations as a flat sequence under the
// statement root rather than as named lifecycle phases (statement /
// pre-commit / post-commit). The phase boundary is implicit in the
// ordering of `execute N system table mutations transactions` markers.
type DDLOperation struct {
	Op      string   `json:"op"`
	Targets []string `json:"targets,omitempty"`
}

// DDLExplainResult is the structured form of a `EXPLAIN (DDL, SHAPE)`
// result.
//
// Statement is the SQL statement that the schema changer would execute,
// extracted from the leading "Schema change plan for <stmt>;" header.
// Operations is the parsed flat list of operations in execution order.
// RawText is the original multi-line `info` string the cluster
// returned, retained verbatim so the CLI text mode can render output
// exactly as `cockroach sql` would and so agents can re-parse if they
// need detail the structured form drops (e.g. tree connector positions).
//
// Like ExplainResult, DDLExplainResult is only constructed on the
// success path; any failure (query, scan, parse) returns the zero value
// plus an error.
type DDLExplainResult struct {
	Statement  string         `json:"statement"`
	Operations []DDLOperation `json:"operations"`
	RawText    string         `json:"raw_text"`
}

// ddlPlanHeaderPrefix is the literal prefix CRDB emits on the first
// line of EXPLAIN (DDL, SHAPE) output. We require it to match (rather
// than fuzzy-detecting an SQL fragment) so a future CRDB format change
// surfaces here as a clear parser error instead of a silently-empty
// Statement field.
const ddlPlanHeaderPrefix = "Schema change plan for "

// parseExplainDDLShape converts the multi-line `info` string of a CRDB
// `EXPLAIN (DDL, SHAPE)` result into structured form.
//
// Inputs:
//   - text: the contents of the single `info` column of the SHAPE
//     result. EXPLAIN (DDL, SHAPE) returns one row whose value is a
//     newline-separated rendering of the schema-change plan.
//
// Outputs:
//   - statement: the SQL statement the schema changer would execute,
//     stripped of the "Schema change plan for " prefix and trailing
//     semicolon. Sensitive literal values remain wrapped in CRDB's
//     `‹...›` redaction markers; callers that need a clean SQL string
//     should strip them downstream.
//   - operations: the flat operation sequence in execution order.
//   - err: non-nil only when the input does not match the SHAPE format
//     (empty input, missing header, unrecognized tree-connector
//     prefixes, target lines before any operation). Unrecognized lines
//     fail loudly so a CRDB upgrade that introduces new SHAPE syntax —
//     including deeper nesting than depth 2 — surfaces here rather
//     than producing a quietly-incomplete plan.
//
// Format reference (from samples captured against cockroach demo):
//
//	Schema change plan for ALTER TABLE t ADD COLUMN x INT;
//	 ├── execute 2 system table mutations transactions
//	 ├── backfill using primary index t_pkey- in relation t
//	 │    └── into t_pkey+ (id; x+)
//	 └── execute 4 system table mutations transactions
//
// Operations are at depth 1 (one space, then `├──` or `└──`). Targets
// are at depth 2 (one space, then `│`-or-space + spaces + `└──`/`├──`).
func parseExplainDDLShape(text string) (string, []DDLOperation, error) {
	if text == "" {
		return "", nil, fmt.Errorf("parse SHAPE output: empty input")
	}

	// CRDB appends a trailing newline; strip it so the trailing empty
	// element doesn't show up as a malformed line below.
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")

	header := lines[0]
	if !strings.HasPrefix(header, ddlPlanHeaderPrefix) {
		return "", nil, fmt.Errorf("parse SHAPE output: first line missing %q prefix: %q", ddlPlanHeaderPrefix, header)
	}
	statement := strings.TrimSuffix(strings.TrimPrefix(header, ddlPlanHeaderPrefix), ";")

	var operations []DDLOperation
	for i, raw := range lines[1:] {
		line := strings.TrimRight(raw, " \t")
		if line == "" {
			continue
		}

		depth, content, err := classifyShapeLine(line)
		if err != nil {
			// +2 so the index in the error matches the line number in
			// the raw text (1-based, with the header counted).
			return "", nil, fmt.Errorf("parse SHAPE output: line %d: %w (raw: %q)", i+2, err, raw)
		}

		switch depth {
		case shapeDepthOperation:
			operations = append(operations, DDLOperation{Op: content})
		case shapeDepthTarget:
			if len(operations) == 0 {
				return "", nil, fmt.Errorf("parse SHAPE output: line %d: target before any operation (raw: %q)", i+2, raw)
			}
			last := &operations[len(operations)-1]
			last.Targets = append(last.Targets, content)
		}
	}

	return statement, operations, nil
}

// shapeDepth identifies the structural role of a SHAPE line. Two levels
// have been observed in real cluster output: an "operation" directly
// under the plan root, and an optional "target" sub-line under an
// operation. classifyShapeLine returns one of these or an error if the
// line's connector pattern doesn't match either.
type shapeDepth int

const (
	shapeDepthOperation shapeDepth = iota + 1
	shapeDepthTarget
)

// classifyShapeLine inspects a single non-empty SHAPE line and returns
// its depth and the content text after the tree connector. The two
// recognized shapes are:
//
//	" ├── <op>"     or  " └── <op>"           → operation (depth 1)
//	" │    └── <t>" or  "      └── <t>"       → target (depth 2)
//	" │    ├── <t>" or  "      ├── <t>"       → target (depth 2)
//
// Any other prefix is a format we have not seen — return an error so
// CRDB-side changes are not silently misclassified. The exact leading
// whitespace counts come from the captured fixtures: depth 1 is one
// space then a connector; depth 2 is one space, then `│` or space, then
// four spaces, then a connector.
func classifyShapeLine(line string) (shapeDepth, string, error) {
	if op, ok := stripConnector(line, " "); ok {
		return shapeDepthOperation, op, nil
	}
	for _, prefix := range []string{" │    ", "      "} {
		if t, ok := stripConnector(line, prefix); ok {
			return shapeDepthTarget, t, nil
		}
	}
	return 0, "", fmt.Errorf("unrecognized tree-connector prefix")
}

// stripConnector returns (content, true) if line begins with prefix
// followed by a `├── ` or `└── ` connector and has non-empty content
// after it. Returns ("", false) otherwise. Centralizing the connector
// match keeps classifyShapeLine readable when we add more depths later.
func stripConnector(line, prefix string) (string, bool) {
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	rest := line[len(prefix):]
	for _, connector := range []string{"├── ", "└── "} {
		if strings.HasPrefix(rest, connector) {
			content := strings.TrimSpace(rest[len(connector):])
			if content == "" {
				return "", false
			}
			return content, true
		}
	}
	return "", false
}
