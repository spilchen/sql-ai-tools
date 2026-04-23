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
// Checks payload without translation.
type Result struct {
	TypeCheck      validateresult.CheckStatus
	NameResolution validateresult.CheckStatus
}

// Run executes every semantic check against the parsed input and
// returns the per-phase Result together with the accumulated
// diagnostics in a stable phase order: type errors, then table-name
// errors, then column-name errors. Errors from later phases are not
// suppressed by errors in earlier phases — the runner exists so a
// single invocation surfaces every diagnostic the user could fix in
// one editing pass.
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
// does this).
//
// Inputs are not copied: stmts and sql are read by the underlying
// checkers and must remain valid for the duration of the call.
func Run(stmts statements.Statements, sql string, cat *catalog.Catalog) (Result, []output.Error) {
	var errs []output.Error

	res := Result{TypeCheck: validateresult.CheckOK}
	if typeErrs := CheckExprTypes(stmts, sql); len(typeErrs) > 0 {
		errs = append(errs, typeErrs...)
		res.TypeCheck = validateresult.CheckFailed
	}

	if cat == nil {
		res.NameResolution = validateresult.CheckSkipped
		return res, errs
	}

	res.NameResolution = validateresult.CheckOK
	tableErrs := CheckTableNames(stmts, sql, cat)
	colErrs := CheckColumnNames(stmts, sql, cat)
	if len(tableErrs) > 0 || len(colErrs) > 0 {
		res.NameResolution = validateresult.CheckFailed
	}
	errs = append(errs, tableErrs...)
	errs = append(errs, colErrs...)
	return res, errs
}
