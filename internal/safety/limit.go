// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package safety

import (
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser/statements"
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
//
// See MaybeInjectLimitParsed for callers (cmd/exec.go,
// internal/mcp/tools/execute.go) that already ran parser.Parse and
// want to skip a second client-side parse.
func MaybeInjectLimit(sql string, maxRows int) (string, bool, error) {
	if maxRows <= 0 {
		return sql, false, nil
	}
	stmts, err := parser.Parse(sql)
	if err != nil {
		return sql, false, err
	}
	rewritten, injected := MaybeInjectLimitParsed(stmts, maxRows)
	if !injected {
		return sql, false, nil
	}
	return rewritten, true, nil
}

// MaybeInjectLimitParsed is the parsed-input variant of
// MaybeInjectLimit. Callers that already invoked parser.Parse use it
// to avoid a second parse. Mirrors summarize.Parsed /
// sqlformat.FormatParsed in shape: the parsed-input variant drops
// the parse-error return since the parse already succeeded upstream.
//
// Return contract: on injection (rewritten, true). On no-injection
// — including the maxRows<=0, multi-statement, non-Select, and
// already-bounded cases — ("", false). Callers MUST keep their own
// SQL string and fall back to it on false; the parsed variant
// deliberately does not round-trip the AST back through
// tree.AsStringWithFlags on the no-injection path so it cannot
// reformat or drop comments. Nil-string-on-false is loud-by-design:
// a caller that writes `rewritten, _ = MaybeInjectLimitParsed(...)`
// would dispatch an empty query rather than fail-silently to the
// original input, surfacing the contract violation immediately.
//
// Ownership: stmts[0].AST is mutated in place when injection fires
// (the slice itself is not retained past return). The same AST
// cannot be safely re-fed to a downstream consumer that expects the
// pre-injection shape, so run version.Inspect / safety.CheckParsed
// on the AST first, then call MaybeInjectLimitParsed last.
func MaybeInjectLimitParsed(stmts statements.Statements, maxRows int) (string, bool) {
	if maxRows <= 0 || len(stmts) != 1 {
		return "", false
	}
	sel, ok := stmts[0].AST.(*tree.Select)
	if !ok || selectIsBounded(sel) {
		return "", false
	}
	if sel.Limit == nil {
		sel.Limit = &tree.Limit{Count: tree.NewDInt(tree.DInt(maxRows))}
	} else {
		// Preserve any existing OFFSET (the only field that can be set
		// when selectIsBounded returns false) and only add the Count.
		sel.Limit.Count = tree.NewDInt(tree.DInt(maxRows))
	}
	return tree.AsStringWithFlags(sel, tree.FmtSimple), true
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
