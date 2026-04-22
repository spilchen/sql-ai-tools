// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package conn

import (
	"fmt"
	"strings"
)

// PlanNode is one operator in a CockroachDB EXPLAIN tree. Op is the
// operator name with the leading bullet stripped (e.g. "scan", "filter",
// "render"). Attrs holds the per-operator key/value attributes the
// planner emits underneath the operator (e.g. "table": "t@primary",
// "spans": "FULL SCAN"). Children are the direct child operators in
// execution order, following EXPLAIN's tree-drawing glyphs.
//
// Both Attrs and Children use omitempty so leaf nodes and attribute-free
// operators serialize to a compact JSON shape.
type PlanNode struct {
	Op       string            `json:"op"`
	Attrs    map[string]string `json:"attrs,omitempty"`
	Children []PlanNode        `json:"children,omitempty"`
}

// parseExplainTree converts the tabular `info` column rows of a CRDB
// EXPLAIN result into structured form.
//
// Inputs:
//   - rows: the contents of EXPLAIN's single `info` column, one slice
//     entry per result row, in the order the cluster returned them.
//
// Outputs:
//   - header: the leading `key: value` lines (typically `distribution`
//     and `vectorized`) that precede the operator tree. Nil when the
//     plan has no header.
//   - plan: the operator forest. Most plans have one root, but defensive
//     [] avoids assuming uniqueness.
//   - err: non-nil only when a row's structure cannot be classified.
//     Unrecognized rows fail loudly rather than getting silently
//     dropped, so a CRDB upgrade that adds new EXPLAIN syntax surfaces
//     here instead of producing a quietly-incomplete tree.
//
// Format reference: CRDB EXPLAIN emits a tree using the glyphs `•`
// (operator marker), `│` (vertical continuation), `└──`/`├──` (last and
// non-last child connectors). Header rows appear first, optionally
// terminated by a `·` separator, then the operator tree. An operator at
// rune-column D has its attribute lines indented two columns further
// (D+2) and its child operators four columns further (D+4). Attribute
// owners are resolved by column rather than by recency so that
// continuation pipes from ancestors (e.g. `│     table: t1`, where the
// pipe belongs to a still-open join above) do not steal attributes from
// the most recently opened operator.
func parseExplainTree(rows []string) (map[string]string, []PlanNode, error) {
	if len(rows) == 0 {
		return nil, nil, nil
	}

	var (
		header   map[string]string
		roots    []PlanNode
		stack    []*stackEntry
		inHeader = true
	)

	for i, raw := range rows {
		line := strings.TrimRight(raw, " \t")
		if line == "" || strings.TrimSpace(line) == "·" {
			// Blank lines and the `·` separator end the header phase
			// and otherwise act as visual separators inside the tree.
			inHeader = false
			continue
		}

		if opCol, op, ok := parseOperatorLine(line); ok {
			inHeader = false
			if op == "" {
				return nil, nil, fmt.Errorf("explain row %d: bullet without operator name: %q", i, raw)
			}
			node := PlanNode{Op: op}
			// Pop entries at or deeper than this column; the new
			// operator either replaces a sibling at the same column
			// or starts a deeper subtree.
			for len(stack) > 0 && stack[len(stack)-1].col >= opCol {
				stack = stack[:len(stack)-1]
			}
			if len(stack) == 0 {
				roots = append(roots, node)
				stack = append(stack, &stackEntry{col: opCol, node: &roots[len(roots)-1]})
			} else {
				parent := stack[len(stack)-1].node
				parent.Children = append(parent.Children, node)
				stack = append(stack, &stackEntry{
					col:  opCol,
					node: &parent.Children[len(parent.Children)-1],
				})
			}
			continue
		}

		contentCol, content := findContent(line)
		if content == "" {
			// Pure prefix (e.g. a `│` continuation row); skip.
			continue
		}

		if inHeader {
			k, v, ok := splitKV(content)
			if !ok {
				return nil, nil, fmt.Errorf("explain row %d: header line is not key: value: %q", i, raw)
			}
			if header == nil {
				header = make(map[string]string)
			}
			header[k] = v
			continue
		}

		owner := ownerByColumn(stack, contentCol-2)
		if owner == nil {
			return nil, nil, fmt.Errorf(
				"explain row %d: attribute at column %d has no open operator at column %d (raw: %q)",
				i, contentCol, contentCol-2, raw,
			)
		}
		k, v, ok := splitKV(content)
		if !ok {
			return nil, nil, fmt.Errorf("explain row %d: attribute is not key: value: %q", i, raw)
		}
		if owner.Attrs == nil {
			owner.Attrs = make(map[string]string)
		}
		owner.Attrs[k] = v
	}

	return header, roots, nil
}

// stackEntry records one open operator on the depth stack. col is the
// rune column at which its `•` glyph appears; node is a pointer into the
// growing tree so further attributes and children can be attached.
type stackEntry struct {
	col  int
	node *PlanNode
}

// isTreeGlyph reports whether r is one of the rune-wide glyphs CRDB uses
// to draw the operator tree. Treating spaces as glyphs lets the parser
// walk a mixed prefix like `│     table:` or `└── • scan` without caring
// about the order of its components.
func isTreeGlyph(r rune) bool {
	switch r {
	case ' ', '\t', '│', '└', '├', '─':
		return true
	}
	return false
}

// parseOperatorLine returns (col, op, true) if line is a `<prefix>• <op>`
// operator row, where col is the rune column of the `•` glyph. It
// returns ("", false) if any non-glyph rune precedes the bullet, or if
// no bullet is present at all — those are attribute or header rows that
// the caller handles separately.
func parseOperatorLine(line string) (int, string, bool) {
	for i, r := range []rune(line) {
		if r == '•' {
			return i, strings.TrimSpace(string([]rune(line)[i+1:])), true
		}
		if !isTreeGlyph(r) {
			return 0, "", false
		}
	}
	return 0, "", false
}

// findContent walks past leading tree glyphs and whitespace and returns
// the rune column of the first content rune together with the trimmed
// content. When the entire line is glyphs/whitespace the column is -1
// and the content is empty (a continuation-only row that the parser
// skips).
func findContent(line string) (int, string) {
	runes := []rune(line)
	for i, r := range runes {
		if isTreeGlyph(r) {
			continue
		}
		return i, strings.TrimSpace(string(runes[i:]))
	}
	return -1, ""
}

// ownerByColumn returns the open operator whose `•` is at the requested
// column, or nil if none is open at that column. Search is from the top
// of the stack so the most deeply nested matching operator wins (matters
// only if a future change ever pushes duplicates at the same column).
func ownerByColumn(stack []*stackEntry, col int) *PlanNode {
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i].col == col {
			return stack[i].node
		}
	}
	return nil
}

// splitKV splits "key: value" on the first colon, trimming whitespace
// around both halves. Returns false when no colon is present so the
// caller can decide whether to treat that as an error or skip.
func splitKV(s string) (string, string, bool) {
	i := strings.Index(s, ":")
	if i < 0 {
		return "", "", false
	}
	return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:]), true
}
