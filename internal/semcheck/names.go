// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package semcheck

import (
	"fmt"
	"strings"
	"sync"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser/statements"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/tree"

	"github.com/spilchen/sql-ai-tools/internal/catalog"
	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/output"
)

// unknownTableCode is the SQLSTATE for "undefined_table". CockroachDB
// itself emits this code for missing-table errors, so agents can branch
// on it without parsing the message.
const unknownTableCode = "42P01"

// CheckTableNames walks every statement in stmts and reports table
// references that do not resolve in cat. CTE names from WITH clauses
// and table aliases are added to a per-statement scope so they are not
// flagged.
//
// Errors are deduplicated by name across the whole input — a single
// missing table referenced from two statements produces one error, not
// two. The first reference's source position is reported.
//
// fullSQL is the original SQL text used to compute 1-based line/column
// positions. cat must be non-nil; the caller decides whether to call
// this based on whether a catalog is available.
func CheckTableNames(stmts statements.Statements, fullSQL string, cat *catalog.Catalog) []output.Error {
	if cat == nil || len(stmts) == 0 {
		return nil
	}

	var errs []output.Error
	seen := make(map[string]struct{})
	availableTables := sync.OnceValue(cat.TableNames)

	// stmtStart advances through fullSQL so each statement's
	// position lookups search only within its own slice. Without
	// this, "SELECT * FROM users u; SELECT * FROM u" would resolve
	// "u" to the substring inside "users".
	stmtStart := 0
	for i := range stmts {
		stmt := stmts[i].AST
		if stmt == nil {
			continue
		}
		stmtSQL := stmts[i].SQL
		stmtEnd := stmtStart + len(stmtSQL)
		if rel := strings.Index(fullSQL[stmtStart:], stmtSQL); rel >= 0 {
			stmtStart += rel
			stmtEnd = stmtStart + len(stmtSQL)
		}

		v := &tableNameVisitor{
			cat:             cat,
			fullSQL:         fullSQL,
			stmtStart:       stmtStart,
			stmtEnd:         stmtEnd,
			availableTables: availableTables,
			scope:           newScope(),
			seen:            seen,
			errs:            &errs,
		}
		tree.WalkStmt(v, stmt)

		stmtStart = stmtEnd
	}
	return errs
}

// tableNameVisitor walks one top-level statement collecting unresolved
// table references.
//
// Lifecycle: one visitor per statement; created by CheckTableNames and
// discarded once WalkStmt returns. seen is shared across statements so
// the same missing name only produces one error per input. scope is
// per-statement so CTE names from one statement do not leak into the
// next. stmtStart/stmtEnd bound the byte range searched by positionFor.
//
// The visitor implements tree.ExtendedVisitor — required because a plain
// tree.Visitor skips TableExpr subtrees during WalkStmt.
type tableNameVisitor struct {
	cat             *catalog.Catalog
	fullSQL         string
	stmtStart       int
	stmtEnd         int
	availableTables func() []string
	scope           *nameScope
	seen            map[string]struct{}
	errs            *[]output.Error
}

var _ tree.ExtendedVisitor = (*tableNameVisitor)(nil)

func (v *tableNameVisitor) VisitPre(expr tree.Expr) (bool, tree.Expr)          { return true, expr }
func (v *tableNameVisitor) VisitPost(expr tree.Expr) tree.Expr                 { return expr }
func (v *tableNameVisitor) VisitTablePost(e tree.TableExpr) tree.TableExpr     { return e }
func (v *tableNameVisitor) VisitStatementPost(s tree.Statement) tree.Statement { return s }

func (v *tableNameVisitor) VisitTablePre(expr tree.TableExpr) (bool, tree.TableExpr) {
	switch t := expr.(type) {
	case *tree.AliasedTableExpr:
		// Bind the alias before returning so it is in scope when
		// walkTableExpr recurses into expr.Expr (e.g. a subquery
		// in FROM that references the alias via a lateral join).
		if t.As.Alias != "" {
			v.scope.add(string(t.As.Alias))
		}
		return true, expr
	case *tree.TableName:
		v.checkTable(t)
		return false, expr
	case *tree.TableRef:
		// Numeric "[123 AS t]" form; no name to resolve.
		return false, expr
	}
	return true, expr
}

func (v *tableNameVisitor) VisitStatementPre(stmt tree.Statement) (bool, tree.Statement) {
	// Bind CTE names before visiting the body so references to them
	// are not flagged. A flat scope (rather than nested frames) is
	// sufficient because CTE/alias names within a statement are
	// visible everywhere downstream of their declaration.
	if w := withClause(stmt); w != nil {
		for _, cte := range w.CTEList {
			if cte == nil {
				continue
			}
			if name := string(cte.Name.Alias); name != "" {
				v.scope.add(name)
			}
		}
	}
	return true, stmt
}

// checkTable reports tn as unknown when it is neither in the
// per-statement scope nor in the catalog. Schema/database qualifiers
// on tn are ignored — the catalog is single-level and tn.Table()
// returns the bare object name.
func (v *tableNameVisitor) checkTable(tn *tree.TableName) {
	name := tn.Table()
	if name == "" || v.scope.has(name) {
		return
	}
	if _, ok := v.cat.Table(name); ok {
		return
	}

	key := strings.ToLower(name)
	if _, dup := v.seen[key]; dup {
		return
	}
	v.seen[key] = struct{}{}

	*v.errs = append(*v.errs, output.Error{
		Code:     unknownTableCode,
		Severity: output.SeverityError,
		Message:  fmt.Sprintf("relation %q does not exist", name),
		Position: v.positionFor(name),
		Category: diag.CategoryUnknownTable,
		Context: map[string]any{
			"available_tables": v.availableTables(),
		},
	})
}

// positionFor locates name within the current statement's byte range
// and returns its 1-based position. Identifier word boundaries are
// enforced so a search for "u" does not match the "u" inside "users".
// Returns nil when the name cannot be found within the statement's
// range — typically because the parser rendered a delimited or
// case-folded identifier that does not appear verbatim in the source.
func (v *tableNameVisitor) positionFor(name string) *output.Position {
	if name == "" {
		return nil
	}
	stmtText := v.fullSQL[v.stmtStart:v.stmtEnd]
	for off := 0; off < len(stmtText); {
		rel := indexFold(stmtText[off:], name)
		if rel < 0 {
			return nil
		}
		abs := v.stmtStart + off + rel
		if isWordBoundary(v.fullSQL, abs, abs+len(name)) {
			return diag.PositionFromByteOffset(v.fullSQL, abs)
		}
		off += rel + 1
	}
	return nil
}

// indexFold is strings.Index with ASCII case folding. Catalog lookups
// are case-insensitive, so position lookups must be too.
func indexFold(haystack, needle string) int {
	return strings.Index(strings.ToLower(haystack), strings.ToLower(needle))
}

// isWordBoundary reports whether [start, end) in s is bordered by
// non-identifier bytes (or string boundaries). An identifier byte is
// ASCII letter/digit/underscore — sufficient for SQL identifiers in
// the unquoted contexts the substring search applies to.
func isWordBoundary(s string, start, end int) bool {
	return !isIdentByte(byteAt(s, start-1)) && !isIdentByte(byteAt(s, end))
}

func byteAt(s string, i int) byte {
	if i < 0 || i >= len(s) {
		return 0
	}
	return s[i]
}

func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

// nameScope is a case-insensitive set of identifiers that should not
// be reported as unknown tables (CTE names and table aliases). The
// wrapper enforces case-folding at the API boundary so callers cannot
// accidentally bypass it.
type nameScope struct {
	names map[string]struct{}
}

func newScope() *nameScope {
	return &nameScope{names: make(map[string]struct{})}
}

func (s *nameScope) add(name string) {
	s.names[strings.ToLower(name)] = struct{}{}
}

func (s *nameScope) has(name string) bool {
	_, ok := s.names[strings.ToLower(name)]
	return ok
}

func withClause(stmt tree.Statement) *tree.With {
	switch s := stmt.(type) {
	case *tree.Select:
		return s.With
	case *tree.Insert:
		return s.With
	case *tree.Update:
		return s.With
	case *tree.Delete:
		return s.With
	}
	return nil
}
