// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package semcheck

import (
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser/statements"

	"github.com/spilchen/sql-ai-tools/internal/catalog"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/validateresult"
)

// Result reports the per-phase outcome of a Run. The fields mirror
// validateresult.Checks (minus Syntax, which is the parser's
// responsibility) so callers can copy directly into the envelope's
// Checks payload without translation. Field declaration order
// matches Run's phase-execution order.
type Result struct {
	FunctionResolution validateresult.CheckStatus
	TypeCheck          validateresult.CheckStatus
	NameResolution     validateresult.CheckStatus
}

// Run executes every semantic check against the parsed input and
// returns the per-phase Result together with the accumulated
// diagnostics in a stable phase order: function-name errors, then
// type errors, then table-name errors, then column-name errors.
// Errors from later phases are not suppressed by errors in earlier
// phases — the runner exists so a single invocation surfaces every
// diagnostic the user could fix in one editing pass.
//
// Function-name resolution runs first because its diagnostics are the
// most actionable output for the common typo case, and because
// CheckExprTypes deliberately skips FuncExprs whose bare names are
// unresolved (see containsUnknownFunc in expr.go) so the user sees
// exactly one structured 42883 per typo rather than a duplicate from
// the type checker.
//
// Cascade suppression for downstream column references against an
// unresolved table is the responsibility of CheckColumnNames (see
// unknownSource in names.go), not the runner; Run simply chains the
// existing checkers.
//
// cat may be nil. When nil, name resolution is skipped and
// NameResolution is reported as CheckSkipped — the caller decides
// whether to also append a capability_required warning to the
// envelope (today only the CLI's --schema-less single-input path
// does this). Function-name resolution does not need a catalog (the
// builtin registry is process-global) and always runs.
//
// Inputs are not copied: stmts and sql are read by the underlying
// checkers and must remain valid for the duration of the call.
func Run(stmts statements.Statements, sql string, cat *catalog.Catalog) (Result, []output.Error) {
	var errs []output.Error

	// Initialize all phases to CheckOK up front. Each phase downgrades
	// its own field on failure, and the cat==nil branch overrides
	// NameResolution to CheckSkipped. Initializing here (rather than
	// in branches) prevents the empty-string zero value from leaking
	// out through any future early-return path.
	res := Result{
		FunctionResolution: validateresult.CheckOK,
		TypeCheck:          validateresult.CheckOK,
		NameResolution:     validateresult.CheckOK,
	}

	if funcErrs := CheckFunctionNames(stmts, sql); len(funcErrs) > 0 {
		errs = append(errs, funcErrs...)
		res.FunctionResolution = validateresult.CheckFailed
	}

	if typeErrs := CheckExprTypes(stmts, sql); len(typeErrs) > 0 {
		errs = append(errs, typeErrs...)
		res.TypeCheck = validateresult.CheckFailed
	}

	if cat == nil {
		res.NameResolution = validateresult.CheckSkipped
		return res, errs
	}

	tableErrs := CheckTableNames(stmts, sql, cat)
	colErrs := CheckColumnNames(stmts, sql, cat)
	if len(tableErrs) > 0 || len(colErrs) > 0 {
		res.NameResolution = validateresult.CheckFailed
	}
	errs = append(errs, tableErrs...)
	errs = append(errs, colErrs...)
	return res, errs
}
