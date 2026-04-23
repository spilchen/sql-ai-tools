// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package semcheck provides Tier 1 (zero-config) semantic checks that
// operate on parsed ASTs without a schema catalog or cluster
// connection. Today there are two zero-config phases: function-name
// resolution (CheckFunctionNames, which surfaces 42883 typos with
// "did you mean?" suggestions) and expression type checking
// (CheckExprTypes, which detects type mismatches in literal
// expressions and builtin function calls using MakeSemaContext(nil)).
// Builtin function metadata is registered at startup via
// internal/builtinstubs, enabling both phases to operate without a
// live database.
//
// Expressions that reference columns, subqueries, placeholders, or
// FuncExprs whose bare names are unknown to the builtin registry are
// silently skipped by CheckExprTypes. Columns/subqueries/placeholders
// are skipped because resolving them requires a catalog (Tier 2) or
// connection (Tier 3); unknown function names are skipped to avoid
// double-reporting the structured 42883 already produced by
// CheckFunctionNames.
package semcheck

import (
	"context"
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser/statements"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/types"

	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/output"
)

// CheckExprTypes walks every expression in stmts and type-checks
// variable-free sub-trees using MakeSemaContext(nil). Expressions
// containing column references, subqueries, or placeholders are
// skipped. Builtin function calls are resolved against the registered
// stubs. Returns one output.Error per type-check failure; the slice is
// nil when all checked expressions are valid.
func CheckExprTypes(stmts statements.Statements, fullSQL string) []output.Error {
	semaCtx := tree.MakeSemaContext(nil)
	var errs []output.Error

	for i := range stmts {
		stmt := stmts[i].AST
		if stmt == nil {
			continue
		}
		// SimpleStmtVisit's error is always nil here because the
		// callback never returns one; we accumulate errors in errs.
		_, _ = tree.SimpleStmtVisit(stmt, func(expr tree.Expr) (bool, tree.Expr, error) {
			if isVariable(expr) {
				return false, expr, nil
			}
			if containsVariable(expr) {
				return true, expr, nil
			}
			// CheckFunctionNames already owns the 42883 diagnostic
			// for bare-unknown calls; type-checking the same node
			// would emit a second error pointing at the same span.
			// Recurse without checking so any well-formed sibling
			// expressions still get type-checked.
			if containsUnknownFunc(expr) {
				return true, expr, nil
			}

			typedExpr, err := safeTypeCheck(expr, &semaCtx)
			if err != nil {
				errs = append(errs, diag.FromTypeError(err, exprText(typedExpr, expr), fullSQL))
			}
			return false, expr, nil
		})
	}
	return errs
}

// isVariable returns true if expr is a node type that requires a
// resolver we don't have in Tier 1 (no catalog). FuncExpr is NOT
// listed here because builtin function stubs are registered at init
// time via internal/builtinstubs, so the type checker can resolve
// builtin function names and check argument types.
func isVariable(expr tree.Expr) bool {
	switch expr.(type) {
	case *tree.UnresolvedName,
		*tree.ColumnItem,
		*tree.Subquery,
		*tree.Placeholder,
		*tree.IndexedVar,
		tree.UnqualifiedStar,
		*tree.AllColumnsSelector:
		return true
	}
	return false
}

// containsVariable walks expr and returns true if any descendant is a
// variable node.
func containsVariable(expr tree.Expr) bool {
	var found bool
	v := variableDetector{found: &found}
	tree.WalkExprConst(&v, expr)
	return found
}

type variableDetector struct {
	found *bool
}

func (d *variableDetector) VisitPre(expr tree.Expr) (bool, tree.Expr) {
	if *d.found {
		return false, expr
	}
	if isVariable(expr) {
		*d.found = true
		return false, expr
	}
	return true, expr
}

func (d *variableDetector) VisitPost(expr tree.Expr) tree.Expr { return expr }

// containsUnknownFunc walks expr and returns true if any descendant is
// a *tree.FuncExpr whose bare name (the rightmost UnresolvedName Part)
// is not registered in tree.FunDefs. Schema-qualified calls and
// pre-resolved references (FunctionDefinition / ResolvedFunction-
// Definition / FunctionOID — produced by grammar rules like
// WrapFunction for keywords) are not counted because CheckFunctionNames
// also skips them.
func containsUnknownFunc(expr tree.Expr) bool {
	var found bool
	v := unknownFuncDetector{found: &found}
	tree.WalkExprConst(&v, expr)
	return found
}

type unknownFuncDetector struct {
	found *bool
}

func (d *unknownFuncDetector) VisitPre(expr tree.Expr) (bool, tree.Expr) {
	if *d.found {
		return false, expr
	}
	fn, ok := expr.(*tree.FuncExpr)
	if !ok {
		return true, expr
	}
	un, ok := fn.Func.FunctionReference.(*tree.UnresolvedName)
	if !ok {
		return true, expr
	}
	if un.NumParts != 1 {
		return true, expr
	}
	name := strings.ToLower(un.Parts[0])
	if name == "" {
		return true, expr
	}
	if _, known := tree.FunDefs[name]; !known {
		*d.found = true
		return false, expr
	}
	return true, expr
}

func (d *unknownFuncDetector) VisitPost(expr tree.Expr) tree.Expr { return expr }

// safeTypeCheck type-checks expr, recovering from panics that the
// type-checker may trigger for certain expression types (e.g.
// GREATEST, LEAST) when the builtins registry is not populated. A
// recovered panic is surfaced as an error so it appears in
// diagnostics rather than being silently swallowed.
func safeTypeCheck(expr tree.Expr, semaCtx *tree.SemaContext) (typed tree.TypedExpr, err error) {
	defer func() {
		if r := recover(); r != nil {
			typed = nil
			err = fmt.Errorf("internal: type-check panicked: %v", r)
		}
	}()
	typed, err = expr.TypeCheck(context.Background(), semaCtx, types.Any)
	return typed, err
}

// exprText returns a string representation of the expression suitable
// for position lookup in the original SQL. It prefers the original
// (pre-type-check) form because the type checker may reformat
// expressions (adding casts, normalizing whitespace), making the
// typed form less likely to match via substring search.
func exprText(typed tree.TypedExpr, original tree.Expr) string {
	if original != nil {
		if s := fmt.Sprint(original); s != "" {
			return s
		}
	}
	if typed != nil {
		if s := typed.String(); s != "" {
			return s
		}
	}
	return ""
}
