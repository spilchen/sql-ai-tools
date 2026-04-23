// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package safety

import (
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/tree"
)

// MaybeInjectLimit returns sql with an injected LIMIT clause when the
// input is a single bare SELECT without a LIMIT. The boolean reports
// whether an injection happened so the caller can surface it in the
// result envelope (transparency: agents must know the cluster did not
// return all rows).
//
// The rewriter is conservative — it only fires when:
//
//   - maxRows > 0 (zero or negative means "unlimited", so skip).
//   - sql parses to exactly one statement (multi-statement batches are
//     left alone; it's not obvious which one to bound).
//   - the statement is *tree.Select whose result is unbounded (no
//     existing Count and no LIMIT ALL — see selectIsBounded). Note
//     that tree.Limit holds both Count and Offset, so a SELECT with
//     OFFSET but no LIMIT still has a non-nil Limit pointer; the
//     bounded check looks at Count specifically rather than the
//     pointer's nil-ness.
//
// Anything else — DML, DDL, parse errors, EXPLAIN wrappers — returns
// the input unchanged with injected=false. Callers that already ran
// safety.Check can treat the error path as unreachable; we still
// surface parser errors rather than silently swallowing them so a
// caller skipping Check won't get mysterious behaviour.
func MaybeInjectLimit(sql string, maxRows int) (string, bool, error) {
	if maxRows <= 0 {
		return sql, false, nil
	}
	stmts, err := parser.Parse(sql)
	if err != nil {
		return sql, false, err
	}
	if len(stmts) != 1 {
		return sql, false, nil
	}
	sel, ok := stmts[0].AST.(*tree.Select)
	if !ok || selectIsBounded(sel) {
		return sql, false, nil
	}
	if sel.Limit == nil {
		sel.Limit = &tree.Limit{Count: tree.NewDInt(tree.DInt(maxRows))}
	} else {
		// Preserve any existing OFFSET (the only field that can be set
		// when selectIsBounded returns false) and only add the Count.
		sel.Limit.Count = tree.NewDInt(tree.DInt(maxRows))
	}
	return tree.AsStringWithFlags(sel, tree.FmtSimple), true, nil
}

// selectIsBounded reports whether sel already has a row-count cap.
// Either an explicit Count or LIMIT ALL counts as "the caller has
// expressed intent about cardinality"; we respect their bound (or
// explicit unboundedness) rather than overwriting it.
func selectIsBounded(sel *tree.Select) bool {
	if sel.Limit == nil {
		return false
	}
	return sel.Limit.Count != nil || sel.Limit.LimitAll
}
