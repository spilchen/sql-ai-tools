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

// unknownColumnCode is the SQLSTATE for "undefined_column" (PG 42703).
// ambiguousColumnCode is the SQLSTATE for "ambiguous_column" (PG 42702),
// emitted when an unqualified ref resolves to columns in two or more
// in-scope sources.
const (
	unknownColumnCode   = "42703"
	ambiguousColumnCode = "42702"
)

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
// positions. A nil cat or empty stmts returns nil with no work; the
// catalog-required wiring lives in the caller (cmd/validate.go), which
// only invokes this when --schema was supplied.
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

	pos := v.positionFor(name)
	tables := v.availableTables()
	*v.errs = append(*v.errs, output.Error{
		Code:     unknownTableCode,
		Severity: output.SeverityError,
		Message:  fmt.Sprintf("relation %q does not exist", name),
		Position: pos,
		Category: diag.CategoryUnknownTable,
		Context: map[string]any{
			"available_tables": tables,
		},
		Suggestions: diag.Suggest(name, tables, pos),
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

// CheckColumnNames walks every statement in stmts and reports column
// references that do not resolve in the per-statement table scope.
//
// Each query block (SELECT/INSERT/UPDATE/DELETE) gets a scope built
// from its FROM/JOIN/USING/INSERT-target/UPDATE-target sources. Every
// source is one of:
//
//   - a catalog table — its columns are known and refs against it are
//     checked;
//   - a derived source (CTE body or subquery in FROM) — columns are
//     inferred from the body's projection list (or taken from an
//     explicit `AS x(a, b)` alias list when present) and refs are
//     checked against that. When inference is defeated (SELECT *,
//     VALUES, an opaque expression shape) the source is recorded
//     with columns=nil and refs are silently skipped;
//   - an unresolved table (CheckTableNames already flagged it),
//     numeric TableRef, ROWS FROM, or other opaque shape — recorded
//     with columns=nil so refs against it are silently skipped,
//     avoiding a cascade of false-positive unknown_column errors.
//
// Subqueries appearing in expressions (WHERE, SELECT-list, HAVING,
// ...) push a child scope so correlated refs to the outer scope still
// resolve.
//
// Errors are deduplicated by (qualifier, column) across the whole
// input — the same missing column referenced from two statements
// produces one error, not two. The first reference's source position
// is reported. fullSQL is the original SQL text used to compute
// 1-based line/column positions. A nil cat or empty stmts returns
// nil with no work; the catalog-required wiring lives in the caller
// (cmd/validate.go), which only invokes this when --schema was
// supplied.
func CheckColumnNames(stmts statements.Statements, fullSQL string, cat *catalog.Catalog) []output.Error {
	if cat == nil || len(stmts) == 0 {
		return nil
	}

	var errs []output.Error
	seen := make(map[string]struct{})

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

		c := &columnChecker{
			cat:       cat,
			fullSQL:   fullSQL,
			stmtStart: stmtStart,
			stmtEnd:   stmtEnd,
			seen:      seen,
			errs:      &errs,
		}
		c.checkStatement(stmt)

		stmtStart = stmtEnd
	}
	return errs
}

// columnChecker holds the per-statement state for column resolution.
//
// Lifecycle: one checker per statement; created by CheckColumnNames
// and discarded once checkStatement returns. seen is shared across
// statements: Go maps are reference-typed, so the same map header
// copied into each checker dedups across the whole input. errs is a
// pointer to the caller's accumulator slice. stmtStart/stmtEnd bound
// the byte range searched by positionFor, mirroring tableNameVisitor.
type columnChecker struct {
	cat       *catalog.Catalog
	fullSQL   string
	stmtStart int
	stmtEnd   int
	seen      map[string]struct{}
	errs      *[]output.Error
}

// scope is a column-resolution frame for a single query block. Each
// SELECT/INSERT/UPDATE/DELETE creates a new scope linked to its
// enclosing scope via parent so correlated refs walk the chain.
//
// sources holds the FROM/JOIN entries visible in this scope. The list
// preserves source order so available_columns lists in error payloads
// appear in the order the user wrote them.
//
// ctes holds the projected column lists of CTEs declared by the
// enclosing WITH clause, keyed lower-case. Populated by pushCTEs and
// read by lookupCTE; never mutated thereafter. Stored separately from
// sources because a CTE only contributes columns once it is referenced
// in FROM — counting it on both the WITH-frame and the FROM-frame
// would mis-flag unqualified refs as ambiguous. sourceForName consults
// lookupCTE before the catalog so a FROM-clause name that matches a
// CTE is wired to the CTE's columns instead of becoming an unknown
// catalog source. Map values may be nil — that means the CTE is in
// scope but its body defeated inference (SELECT *, VALUES, opaque
// expression) and a downstream FROM-clause reference will record an
// unknownSource so refs against the alias are silently skipped.
//
// selectAliases is populated by SELECT-list parsing for use by GROUP
// BY / HAVING / ORDER BY: an unqualified ref that matches an alias is
// treated as resolved (skipped) to avoid false-positive unknown_column
// errors for SELECT a + b AS sum FROM t ORDER BY sum.
type scope struct {
	parent        *scope
	sources       []source
	ctes          map[string][]string
	selectAliases map[string]struct{}
}

// source is one entry in a scope. alias is the name a qualified
// reference must use — the explicit alias when present, otherwise the
// bare table name; empty alias marks a placeholder that contributes
// only to cascade suppression (no qualified ref can match it).
//
// Invariant: columns == nil means "columns unknown — skip refs against
// this source". A non-nil slice is the authoritative column list, and
// every construction path (catalog tables, inferred derived-source
// projections, explicit AS x(a, b) alias lists) populates at least
// one entry — projection inference is all-or-nothing, so a partial
// projection collapses to nil instead of producing an empty slice.
// Callers MUST use columnsKnown() to test, never len(columns) == 0 —
// the project-wide convention to prefer len-checks over nil-checks
// does not apply to this field.
type source struct {
	alias   string
	columns []string
}

// columnsKnown reports whether the source has authoritative columns.
// All resolution paths gate on this rather than testing columns
// directly, so the nil-vs-empty distinction lives in one place.
func (s source) columnsKnown() bool {
	return s.columns != nil
}

// unknownSource builds a placeholder for any source whose columns we
// cannot enumerate: CTE names, subqueries in FROM, numeric TableRefs,
// catalog-missing tables, ROWS FROM(...), and set-op (UNION/etc.)
// results. Refs against the alias are silently skipped, and any
// scope frame containing one suppresses unknown_column for unmatched
// unqualified refs (the ref might belong to the unknown source).
func unknownSource(alias string) source {
	return source{alias: alias, columns: nil}
}

// columnRef is a single resolved-or-not column reference extracted
// from the AST. qualifier is the table prefix ("t" in t.c) or empty
// for unqualified refs. name is the bare column identifier. They are
// used for both lookup (via aliasMatch / equalFold) and dedup keying.
type columnRef struct {
	qualifier string
	name      string
}

// checkStatement dispatches on the top-level statement type. Each
// supported statement constructs its own root scope (no parent) since
// this is the outermost block; nested SELECTs in subqueries push
// child scopes via walkExpr.
func (c *columnChecker) checkStatement(stmt tree.Statement) {
	switch s := stmt.(type) {
	case *tree.Select:
		c.checkSelect(s, nil)
	case *tree.Insert:
		c.checkInsert(s, nil)
	case *tree.Update:
		c.checkUpdate(s, nil)
	case *tree.Delete:
		c.checkDelete(s, nil)
	}
}

// checkSelect handles a top-level Select node. CTE names from the
// WITH clause are recorded on a dedicated parent frame's ctes map so
// they are visible (via lookupCTE) to both the inner SelectStatement
// and any FROM-subquery inside it. A FROM-clause reference to a CTE
// alias resolves against the CTE's inferred projection; refs to an
// alias whose body defeated inference (SELECT *, VALUES, ...) are
// silently skipped. ORDER BY and LIMIT walk in the same scope as the
// inner SelectClause so they see SELECT-list aliases.
//
// Set-op and ParenSelect results are recorded as unknown placeholder
// sources by checkSelectStatement so ORDER BY refs against the
// combined columns are silently skipped rather than false-positived;
// VALUES blocks have no projection labels and do not get a
// placeholder. Hoisting the union/paren projection into the
// surrounding ORDER BY scope is deliberately out of scope for the
// derived-source inference that pushCTEs and addTableExpr perform.
func (c *columnChecker) checkSelect(sel *tree.Select, parent *scope) {
	parent = c.pushCTEs(parent, sel.With)
	sc := &scope{parent: parent}
	c.checkSelectStatement(sel.Select, sc)
	for _, item := range sel.OrderBy {
		c.walkExpr(item.Expr, sc)
	}
	if sel.Limit != nil {
		c.walkExpr(sel.Limit.Count, sc)
		c.walkExpr(sel.Limit.Offset, sc)
	}
}

// checkSelectStatement dispatches on the SelectStatement variants the
// parser produces. Set operations (UNION/INTERSECT/EXCEPT) check each
// branch independently in the parent scope; the branches do not share
// FROM scope but do share the enclosing CTE/correlation scope.
//
// For set ops and ParenSelect, the result has columns the caller's
// ORDER BY/LIMIT may reference, but we deliberately do NOT hoist the
// union/paren projection into sc here — that scope-crossing inference
// is not implemented for top-level set ops. Instead an unknown
// placeholder is appended to sc, which suppresses unknown_column for
// unmatched refs without falsely accepting them.
func (c *columnChecker) checkSelectStatement(s tree.SelectStatement, sc *scope) {
	switch t := s.(type) {
	case *tree.SelectClause:
		c.checkSelectClause(t, sc)
	case *tree.UnionClause:
		c.checkSelect(t.Left, sc.parent)
		c.checkSelect(t.Right, sc.parent)
		sc.sources = append(sc.sources, unknownSource(""))
	case *tree.ParenSelect:
		c.checkSelect(t.Select, sc.parent)
		sc.sources = append(sc.sources, unknownSource(""))
	case *tree.ValuesClause:
		for _, row := range t.Rows {
			for _, expr := range row {
				c.walkExpr(expr, sc)
			}
		}
	}
}

// checkSelectClause builds the FROM scope, collects SELECT-list
// aliases, then walks every column-bearing clause in this block.
// Order matters: aliases must be in scope before HAVING/GROUP BY
// walk, and FROM sources before any expression walk.
func (c *columnChecker) checkSelectClause(sel *tree.SelectClause, sc *scope) {
	for _, te := range sel.From.Tables {
		c.addTableExpr(sc, te)
	}
	if sc.selectAliases == nil {
		sc.selectAliases = make(map[string]struct{})
	}
	for _, se := range sel.Exprs {
		if se.As != "" {
			sc.selectAliases[strings.ToLower(string(se.As))] = struct{}{}
		}
	}
	for _, se := range sel.Exprs {
		c.walkExpr(se.Expr, sc)
	}
	if sel.Where != nil {
		c.walkExpr(sel.Where.Expr, sc)
	}
	for _, gb := range sel.GroupBy {
		c.walkExpr(gb, sc)
	}
	if sel.Having != nil {
		c.walkExpr(sel.Having.Expr, sc)
	}
}

// checkInsert handles INSERT. The target column list (INSERT INTO t
// (a, b) ...) is checked against the target table; the source rows
// are checked as a Select in a fresh scope so they cannot see the
// target's columns (which is correct: VALUES (...) cannot reference
// the target). ON CONFLICT and RETURNING extensions are walked
// against the target scope.
func (c *columnChecker) checkInsert(ins *tree.Insert, parent *scope) {
	parent = c.pushCTEs(parent, ins.With)
	sc := &scope{parent: parent}

	target := c.targetSource(ins.Table, sc)
	sc.sources = append(sc.sources, target)
	if target.columnsKnown() {
		for _, name := range ins.Columns {
			c.resolveRef(columnRef{qualifier: target.alias, name: string(name)}, sc)
		}
	}
	// When the target's columns are unknown (e.g. INSERT INTO [123 AS
	// t] (a, b) ...), skip ins.Columns resolution entirely — claiming
	// "a does not exist" against an unresolvable target would be a
	// false positive.

	if ins.Rows != nil {
		// Source rows are parented to the CTE frame, not sc itself,
		// so VALUES (...) cannot reference target columns. CTEs from
		// ins.With remain visible because pushCTEs put them on
		// `parent`.
		c.checkSelect(ins.Rows, parent)
	}

	if ins.OnConflict != nil {
		c.walkOnConflict(ins.OnConflict, target, sc)
	}
	c.walkReturning(ins.Returning, sc)
}

// checkUpdate handles UPDATE. The target table is added to the scope
// so SET-LHS column refs and WHERE/SET-RHS expressions can resolve
// against it. Update.From (the optional FROM clause for joins) adds
// extra sources; RETURNING is walked against the same scope.
func (c *columnChecker) checkUpdate(upd *tree.Update, parent *scope) {
	parent = c.pushCTEs(parent, upd.With)
	sc := &scope{parent: parent}
	sc.sources = append(sc.sources, c.targetSource(upd.Table, sc))
	for _, te := range upd.From {
		c.addTableExpr(sc, te)
	}
	for _, ue := range upd.Exprs {
		for _, name := range ue.Names {
			c.resolveRef(columnRef{name: string(name)}, sc)
		}
		c.walkExpr(ue.Expr, sc)
	}
	if upd.Where != nil {
		c.walkExpr(upd.Where.Expr, sc)
	}
	c.walkReturning(upd.Returning, sc)
}

// checkDelete handles DELETE. The target table joins the scope along
// with any USING sources (DELETE FROM users USING orders WHERE ...).
// RETURNING is walked against the same scope.
func (c *columnChecker) checkDelete(del *tree.Delete, parent *scope) {
	parent = c.pushCTEs(parent, del.With)
	sc := &scope{parent: parent}
	sc.sources = append(sc.sources, c.targetSource(del.Table, sc))
	for _, te := range del.Using {
		c.addTableExpr(sc, te)
	}
	if del.Where != nil {
		c.walkExpr(del.Where.Expr, sc)
	}
	c.walkReturning(del.Returning, sc)
}

// walkOnConflict handles INSERT's ON CONFLICT (target_cols) DO UPDATE
// SET ... WHERE ... clause. Conflict-target columns must exist on the
// target table; the SET expressions, predicate, and arbiter predicate
// are walked against an inner scope that adds the special "excluded"
// pseudo-source (whose columns are unknown — refs to excluded.col are
// silently skipped). The target itself is already in sc, so the inner
// scope only adds excluded; that avoids duplicating target and tripping
// the ambiguity check.
//
// SET LHS column names are resolved directly against the target rather
// than via resolveRef, so the cascade-suppression triggered by
// excluded's unknown columns does not also hide a real typo on the
// LHS — provided the target's columns are known. When the target is
// unresolvable (e.g. a TableRef), every ref in the ON CONFLICT body is
// silently skipped along with the rest, by the same cascade-
// suppression rule that already protects unknown FROM sources.
func (c *columnChecker) walkOnConflict(oc *tree.OnConflict, target source, sc *scope) {
	if target.columnsKnown() {
		for _, elem := range oc.Columns {
			c.resolveRef(columnRef{qualifier: target.alias, name: string(elem)}, sc)
		}
	}
	innerScope := &scope{
		parent:  sc,
		sources: []source{unknownSource("excluded")},
	}
	for _, ue := range oc.Exprs {
		if target.columnsKnown() {
			for _, name := range ue.Names {
				c.resolveRef(columnRef{qualifier: target.alias, name: string(name)}, sc)
			}
		}
		c.walkExpr(ue.Expr, innerScope)
	}
	if oc.Where != nil {
		c.walkExpr(oc.Where.Expr, innerScope)
	}
	c.walkExpr(oc.ArbiterPredicate, innerScope)
}

// walkReturning resolves column refs in a RETURNING clause against
// sc. RETURNING permits both bare column refs and arbitrary
// expressions, so each ReturningExpr's expression is walked normally.
// A nil or no-op clause (the common case) is a no-op.
func (c *columnChecker) walkReturning(ret tree.ReturningClause, sc *scope) {
	exprs, ok := ret.(*tree.ReturningExprs)
	if !ok {
		return
	}
	for _, se := range *exprs {
		c.walkExpr(se.Expr, sc)
	}
}

// pushCTEs returns parent unchanged when with is nil; otherwise it
// returns a new frame whose ctes map records each CTE's projected
// column list (or nil when inference was defeated). The frame sits
// above the per-statement scope so that lookupCTE can find each CTE
// by name from any descendant — including FROM-subqueries and
// correlated subqueries.
//
// CTEs are NOT added to frame.sources: a CTE only contributes
// columns when it is actually referenced in a FROM clause, where
// addTableExpr promotes it via lookupCTE. Adding it to sources here
// would double-count under unqualified-ambiguity checks.
//
// Each CTE body is also walked here so refs inside the body (e.g.
// "WITH x AS (SELECT bad FROM users)") are checked against their own
// scope rather than silently skipped.
func (c *columnChecker) pushCTEs(parent *scope, with *tree.With) *scope {
	if with == nil || len(with.CTEList) == 0 {
		return parent
	}
	frame := &scope{parent: parent, ctes: make(map[string][]string)}
	for _, cte := range with.CTEList {
		if cte == nil {
			continue
		}
		name := string(cte.Name.Alias)
		if name != "" {
			frame.ctes[strings.ToLower(name)] = projectedColumns(cte.Name.Cols, selectStatementOf(cte.Stmt))
		}
		if cte.Stmt != nil {
			c.checkStatement(cte.Stmt)
		}
	}
	return frame
}

// targetSource resolves the table appearing in the target slot of an
// INSERT/UPDATE/DELETE. Returns an unknownSource (with the alias the
// user wrote, when available) for non-plain-table targets such as
// numeric TableRefs — refs against those targets are then suppressed
// instead of falsely reported.
func (c *columnChecker) targetSource(te tree.TableExpr, sc *scope) source {
	switch t := te.(type) {
	case *tree.AliasedTableExpr:
		alias := string(t.As.Alias)
		if name, ok := tableNameOf(t.Expr); ok {
			if alias == "" {
				alias = name
			}
			return c.sourceForName(alias, name, sc)
		}
		return unknownSource(alias)
	case *tree.TableName:
		return c.sourceForName(t.Table(), t.Table(), sc)
	case *tree.ParenTableExpr:
		return c.targetSource(t.Expr, sc)
	}
	return unknownSource("")
}

// sourceForTable looks up name in the catalog and returns a source
// with the discovered columns. When the table is missing the source
// is recorded as unknown so refs against it are skipped — the missing
// table is already flagged by CheckTableNames.
func (c *columnChecker) sourceForTable(alias, name string) source {
	tbl, ok := c.cat.Table(name)
	if !ok {
		return unknownSource(alias)
	}
	cols := make([]string, len(tbl.Columns))
	for i, col := range tbl.Columns {
		cols[i] = col.Name
	}
	return source{alias: alias, columns: cols}
}

// sourceForName resolves a TableName-shaped FROM/target reference to
// a source. CTEs in scope take precedence over the catalog so that
// `WITH t AS (...) SELECT * FROM t` wires `t` to the CTE's projection
// instead of a catalog miss. A CTE with nil columns (inference
// defeated) yields an unknownSource — refs against the alias are
// silently skipped, matching pre-#98 behavior for that shape.
func (c *columnChecker) sourceForName(alias, name string, sc *scope) source {
	if cols, ok := lookupCTE(sc, name); ok {
		if cols == nil {
			return unknownSource(alias)
		}
		return source{alias: alias, columns: cols}
	}
	return c.sourceForTable(alias, name)
}

// lookupCTE walks the scope chain returning the projected columns of
// the CTE named name. ok is true iff a CTE with that name is in
// scope; cols may still be nil when the CTE's projection could not be
// inferred (SELECT *, VALUES, etc.). Lookup is case-insensitive to
// match the catalog's identifier folding.
func lookupCTE(sc *scope, name string) ([]string, bool) {
	want := strings.ToLower(name)
	for s := sc; s != nil; s = s.parent {
		if cols, ok := s.ctes[want]; ok {
			return cols, true
		}
	}
	return nil, false
}

// addTableExpr walks a FROM/JOIN/USING TableExpr, appending one
// source per table-shaped leaf. JOIN ON conditions are walked in the
// already-extended scope so both sides are visible to the predicate;
// JOIN USING column names are resolved against that same scope so
// USING (id) catches typos like USING (idd). FROM-subqueries get
// their projection inferred via projectedColumns; opaque sources
// (TableRef, RowsFromExpr, StatementSource) and inference-defeated
// subqueries are recorded as unknownSource so refs against their
// alias are skipped.
func (c *columnChecker) addTableExpr(sc *scope, te tree.TableExpr) {
	switch t := te.(type) {
	case *tree.AliasedTableExpr:
		alias := string(t.As.Alias)
		switch inner := t.Expr.(type) {
		case *tree.TableName:
			name := inner.Table()
			if alias == "" {
				alias = name
			}
			sc.sources = append(sc.sources, c.sourceForName(alias, name, sc))
		case *tree.Subquery:
			// The body is recursed with sc.parent (not sc) so it sees
			// neither its own alias (about to be appended below) nor
			// any sibling FROM items — non-LATERAL FROM subqueries
			// can only reference the enclosing query.
			c.checkSelectStatement(inner.Select, &scope{parent: sc.parent})
			cols := projectedColumns(t.As.Cols, inner.Select)
			if cols == nil {
				sc.sources = append(sc.sources, unknownSource(alias))
			} else {
				sc.sources = append(sc.sources, source{alias: alias, columns: cols})
			}
		case *tree.ParenTableExpr:
			c.addTableExpr(sc, &tree.AliasedTableExpr{Expr: tree.StripTableParens(inner), As: t.As})
		default:
			// TableRef, RowsFromExpr, StatementSource, and any other
			// future TableExpr shapes land here. Record the alias as
			// unknown so qualified refs against it resolve to "skip"
			// rather than "missing FROM-clause entry".
			sc.sources = append(sc.sources, unknownSource(alias))
		}
	case *tree.JoinTableExpr:
		c.addTableExpr(sc, t.Left)
		c.addTableExpr(sc, t.Right)
		switch cond := t.Cond.(type) {
		case *tree.OnJoinCond:
			c.walkExpr(cond.Expr, sc)
		case *tree.UsingJoinCond:
			// USING (col) is the join condition itself, not an
			// expression ref — col existing in both joined sides is
			// the whole point, so the unqualified-ambiguity check
			// would always fire and is wrong here. Just verify the
			// column appears in some in-scope source (or that scope
			// has an unknown source that might contain it); flag it
			// if neither holds.
			for _, name := range cond.Cols {
				colName := string(name)
				if !columnExistsInScope(sc, colName) {
					cols := availableColumns(sc)
					c.report(columnRef{name: colName}, cols, output.Error{
						Code:     unknownColumnCode,
						Severity: output.SeverityError,
						Message:  fmt.Sprintf("column %q does not exist", colName),
						Category: diag.CategoryUnknownColumn,
						Context: map[string]any{
							"available_columns": cols,
						},
					})
				}
			}
		}
	case *tree.TableName:
		sc.sources = append(sc.sources, c.sourceForName(t.Table(), t.Table(), sc))
	case *tree.ParenTableExpr:
		c.addTableExpr(sc, t.Expr)
	default:
		// Bare Subquery / RowsFromExpr / StatementSource without an
		// AliasedTableExpr wrapper. No alias to record; just recurse
		// into anything that might contain refs.
		if sub, ok := te.(*tree.Subquery); ok && sub.Select != nil {
			c.checkSelectStatement(sub.Select, &scope{parent: sc.parent})
		}
		sc.sources = append(sc.sources, unknownSource(""))
	}
}

// walkExpr descends through an expression tree, resolving every
// column reference against sc and recursing into any nested
// subqueries with sc as parent. Stars (* and t.*) are skipped — they
// are not column-name references in the resolution sense.
func (c *columnChecker) walkExpr(expr tree.Expr, sc *scope) {
	if expr == nil {
		return
	}
	v := &exprVisitor{checker: c, scope: sc}
	tree.WalkExprConst(v, expr)
}

// exprVisitor is an Expr-tree visitor wired to the checker. It is
// allocated fresh per walkExpr call so the checker/scope it carries
// match the current frame.
type exprVisitor struct {
	checker *columnChecker
	scope   *scope
}

var _ tree.Visitor = (*exprVisitor)(nil)

func (v *exprVisitor) VisitPre(expr tree.Expr) (bool, tree.Expr) {
	switch n := expr.(type) {
	case *tree.UnresolvedName:
		v.checker.resolveRef(refFromUnresolved(n), v.scope)
		return false, expr
	case *tree.ColumnItem:
		v.checker.resolveRef(refFromColumnItem(n), v.scope)
		return false, expr
	case *tree.Subquery:
		// Push a child scope so correlated refs (e.g. EXISTS (SELECT
		// 1 FROM o WHERE o.x = outer.y)) walk back to outer via the
		// parent pointer. Returning false stops WalkExprConst from
		// re-entering the subquery via Subquery.Walk.
		if n.Select != nil {
			v.checker.checkSelectStatement(n.Select, &scope{parent: v.scope})
		}
		return false, expr
	case tree.UnqualifiedStar, *tree.AllColumnsSelector:
		return false, expr
	}
	return true, expr
}

func (v *exprVisitor) VisitPost(expr tree.Expr) tree.Expr { return expr }

// refFromUnresolved extracts the column qualifier and name from an
// UnresolvedName. UnresolvedName.Parts is stored in reverse order
// (column, table, schema, catalog), so Parts[0] is always the column
// and Parts[1], when present, is the table-or-alias qualifier.
func refFromUnresolved(n *tree.UnresolvedName) columnRef {
	if n.Star || n.NumParts == 0 {
		return columnRef{}
	}
	r := columnRef{name: n.Parts[0]}
	if n.NumParts >= 2 {
		r.qualifier = n.Parts[1]
	}
	return r
}

// refFromColumnItem extracts the column qualifier and name from a
// ColumnItem. Most refs surface as UnresolvedName; ColumnItem appears
// after some normalization passes and is handled here so callers
// don't have to special-case it.
func refFromColumnItem(c *tree.ColumnItem) columnRef {
	r := columnRef{name: string(c.ColumnName)}
	if c.TableName != nil && c.TableName.NumParts >= 1 {
		r.qualifier = c.TableName.Parts[0]
	}
	return r
}

// resolveRef classifies ref against sc and emits at most one error
// per (qualifier, column) pair across the whole input. Outcomes:
//
//   - Empty ref (e.g. extracted from a star) — silently ignored.
//   - Qualified ref against a known source whose columns include the
//     name — resolved, no error.
//   - Qualified ref against an unknown qualifier — unknown_column
//     ("missing FROM-clause entry"); Context lists in-scope tables.
//   - Qualified ref against a known source whose columns do NOT
//     include the name — unknown_column with available_columns from
//     that source.
//   - Unqualified ref matching a SELECT-list alias — resolved (keeps
//     ORDER BY over aliases quiet).
//   - Unqualified ref hitting two or more sources — 42702
//     ambiguous_reference with the matching tables in Context.
//   - Unqualified ref hitting zero sources — unknown_column with the
//     union of all in-scope columns.
//
// Sources with columns unknown (CTE, subquery, unresolved table) are
// treated as "may contain anything" — they suppress every outcome
// except a positive match elsewhere, preventing cascades.
func (c *columnChecker) resolveRef(ref columnRef, sc *scope) {
	if ref.name == "" {
		return
	}
	if ref.qualifier != "" {
		c.resolveQualified(ref, sc)
		return
	}
	c.resolveUnqualified(ref, sc)
}

// resolveQualified handles "t.c" refs. Walks the scope chain looking
// for a source whose alias matches t. If the source's columns are
// known, c must appear in them; otherwise we skip silently.
func (c *columnChecker) resolveQualified(ref columnRef, sc *scope) {
	src, found := lookupSource(sc, ref.qualifier)
	if !found {
		// The typo is the qualifier, not the column. Suggestions and
		// the source-position lookup target the qualifier token.
		aliases := availableAliases(sc)
		c.reportFor(ref.qualifier, ref.qualifier, aliases, output.Error{
			Code:     unknownColumnCode,
			Severity: output.SeverityError,
			Message:  fmt.Sprintf("missing FROM-clause entry for table %q", ref.qualifier),
			Category: diag.CategoryUnknownColumn,
			Context: map[string]any{
				"missing_table":    ref.qualifier,
				"available_tables": aliases,
			},
		})
		return
	}
	if !src.columnsKnown() {
		return
	}
	if containsFold(src.columns, ref.name) {
		return
	}
	c.report(ref, src.columns, output.Error{
		Code:     unknownColumnCode,
		Severity: output.SeverityError,
		Message:  fmt.Sprintf("column %q does not exist", ref.name),
		Category: diag.CategoryUnknownColumn,
		Context: map[string]any{
			"table":             src.alias,
			"available_columns": src.columns,
		},
	})
}

// resolveUnqualified handles bare column refs. Counts the in-scope
// sources that contain the name (sources with unknown columns count
// as "skip" — they do not match and do not error). A SELECT-list
// alias matching name is also a positive resolution to keep ORDER BY
// over aliases quiet.
func (c *columnChecker) resolveUnqualified(ref columnRef, sc *scope) {
	if matchesAlias(sc, ref.name) {
		return
	}
	hasUnknownSource := scopeHasUnknownSource(sc)
	matches := matchingSources(sc, ref.name)
	switch {
	case len(matches) == 1:
		return
	case len(matches) >= 2:
		// Spelling isn't the problem here — the ref matched two real
		// sources. Suggestions would be misleading, so pass nil.
		c.report(ref, nil, output.Error{
			Code:     ambiguousColumnCode,
			Severity: output.SeverityError,
			Message:  fmt.Sprintf("column reference %q is ambiguous", ref.name),
			Category: diag.CategoryAmbiguousReference,
			Context: map[string]any{
				"column": ref.name,
				"tables": matches,
			},
		})
	default:
		if hasUnknownSource {
			// At least one source has unknown columns; the ref might
			// belong to it. Skip rather than false-positive.
			return
		}
		cols := availableColumns(sc)
		c.report(ref, cols, output.Error{
			Code:     unknownColumnCode,
			Severity: output.SeverityError,
			Message:  fmt.Sprintf("column %q does not exist", ref.name),
			Category: diag.CategoryUnknownColumn,
			Context: map[string]any{
				"available_columns": cols,
			},
		})
	}
}

// report is a thin wrapper around reportFor for the common case
// where the misspelled token is the column name and dedup is by
// "qualifier.name". Pass nil candidates to skip the suggestion
// lookup (e.g. ambiguous-ref errors, where spelling is not the
// issue).
func (c *columnChecker) report(ref columnRef, candidates []string, e output.Error) {
	c.reportFor(ref.qualifier+"."+ref.name, ref.name, candidates, e)
}

// reportFor records one error in the accumulator, deduplicating by a
// lowercased dedupKey and attaching the position of misspelled in the
// current statement plus any "did you mean?" suggestions derived from
// candidates.
//
// The dedup key is opaque to reportFor: report passes "qualifier.name"
// for column-not-found errors so two textually-distinct occurrences
// of the same typo collapse to one envelope entry; resolveQualified
// passes the bare qualifier when the qualifier itself is the typo so
// "missing FROM-clause entry for table z" appears once even when
// referenced multiple times.
//
// The reported position is the first non-duplicate occurrence in
// input order — i.e. in the first statement where the dedup key
// appears, not necessarily the textually-first occurrence across
// statements. Position uses the same word-boundary-aware substring
// search as the table walker.
func (c *columnChecker) reportFor(dedupKey, misspelled string, candidates []string, e output.Error) {
	key := strings.ToLower(dedupKey)
	if _, dup := c.seen[key]; dup {
		return
	}
	c.seen[key] = struct{}{}
	e.Position = c.positionFor(misspelled)
	e.Suggestions = diag.Suggest(misspelled, candidates, e.Position)
	*c.errs = append(*c.errs, e)
}

// positionFor mirrors tableNameVisitor.positionFor.
func (c *columnChecker) positionFor(name string) *output.Position {
	if name == "" {
		return nil
	}
	stmtText := c.fullSQL[c.stmtStart:c.stmtEnd]
	for off := 0; off < len(stmtText); {
		rel := indexFold(stmtText[off:], name)
		if rel < 0 {
			return nil
		}
		abs := c.stmtStart + off + rel
		if isWordBoundary(c.fullSQL, abs, abs+len(name)) {
			return diag.PositionFromByteOffset(c.fullSQL, abs)
		}
		off += rel + 1
	}
	return nil
}

// tableNameOf strips down to the bare table name from a TableExpr,
// returning ok=false for derived sources (subqueries, refs, etc.)
// that have no catalog lookup.
func tableNameOf(te tree.TableExpr) (string, bool) {
	switch t := te.(type) {
	case *tree.TableName:
		return t.Table(), true
	case *tree.AliasedTableExpr:
		return tableNameOf(t.Expr)
	case *tree.ParenTableExpr:
		return tableNameOf(t.Expr)
	}
	return "", false
}

// lookupSource walks the scope chain to find a source whose alias
// case-insensitively matches qualifier. Returns the matching source
// and true on hit; zero source and false on miss.
func lookupSource(sc *scope, qualifier string) (source, bool) {
	want := strings.ToLower(qualifier)
	for s := sc; s != nil; s = s.parent {
		for _, src := range s.sources {
			if strings.ToLower(src.alias) == want {
				return src, true
			}
		}
	}
	return source{}, false
}

// matchesAlias reports whether name matches a SELECT-list alias in
// any frame of the scope chain. Used to suppress unknown_column
// errors for ORDER BY/GROUP BY/HAVING refs to SELECT aliases.
func matchesAlias(sc *scope, name string) bool {
	want := strings.ToLower(name)
	for s := sc; s != nil; s = s.parent {
		if _, ok := s.selectAliases[want]; ok {
			return true
		}
	}
	return false
}

// matchingSources returns the alias of every source whose columns
// contain name (case-insensitive). The scope chain is walked across
// all frames — we intentionally count matches more strictly than
// PostgreSQL (which shadows outer scopes) so that a ref which would
// silently bind to an outer column when the user likely meant the
// inner is flagged as ambiguous.
func matchingSources(sc *scope, name string) []string {
	var matches []string
	for s := sc; s != nil; s = s.parent {
		for _, src := range s.sources {
			if !src.columnsKnown() {
				continue
			}
			if containsFold(src.columns, name) {
				matches = append(matches, src.alias)
			}
		}
	}
	return matches
}

// columnExistsInScope reports whether name appears in at least one
// in-scope source's columns, or whether any source has unknown
// columns (in which case we conservatively assume existence to
// suppress cascades). Used by USING-clause resolution where
// unqualified-ambiguity is the wrong outcome — USING (col) requires
// col on both sides by construction.
func columnExistsInScope(sc *scope, name string) bool {
	for s := sc; s != nil; s = s.parent {
		for _, src := range s.sources {
			if !src.columnsKnown() {
				return true
			}
			if containsFold(src.columns, name) {
				return true
			}
		}
	}
	return false
}

// scopeHasUnknownSource reports whether any source in the scope chain
// has unknown columns. When true, an unqualified ref that matches no
// known source is silently skipped rather than reported, because the
// ref might belong to the unknown source (CTE, subquery, unresolved
// table) — flagging it would be a false positive cascade.
func scopeHasUnknownSource(sc *scope) bool {
	for s := sc; s != nil; s = s.parent {
		for _, src := range s.sources {
			if !src.columnsKnown() {
				return true
			}
		}
	}
	return false
}

// availableColumns returns the union of all known column names in
// scope, in source order, with duplicates collapsed. Used to populate
// the available_columns context on unqualified-unknown errors.
func availableColumns(sc *scope) []string {
	var out []string
	seen := make(map[string]struct{})
	for s := sc; s != nil; s = s.parent {
		for _, src := range s.sources {
			for _, col := range src.columns {
				key := strings.ToLower(col)
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
				out = append(out, col)
			}
		}
	}
	return out
}

// availableAliases returns the alias of every source in scope, in
// declaration order. Used as available_tables context when a
// qualified ref names a missing FROM-clause entry.
func availableAliases(sc *scope) []string {
	var out []string
	for s := sc; s != nil; s = s.parent {
		for _, src := range s.sources {
			if src.alias != "" {
				out = append(out, src.alias)
			}
		}
	}
	return out
}

// projectedColumns returns the column names exposed by a derived
// source (a CTE body or FROM-subquery). explicitCols, when non-empty,
// is the user-provided alias list from `WITH cte(a, b) AS (...)` /
// `(SELECT ...) AS x(a, b)` and wins outright. Otherwise the names
// come from inferProjection over sel. A nil sel — used by the CTE
// callsite when the body is not a SELECT (writable CTE, etc.) —
// produces nil unless an explicit alias list overrides it.
//
// Returns nil when the projection cannot be enumerated (SELECT *,
// VALUES, opaque expression, non-SELECT CTE body) — callers must
// then fall back to unknownSource so refs are silently skipped
// instead of false-positive flagged.
func projectedColumns(explicitCols tree.ColumnDefList, sel tree.SelectStatement) []string {
	if cols := explicitColumnNames(explicitCols); cols != nil {
		return cols
	}
	if sel == nil {
		return nil
	}
	return inferProjection(sel)
}

// selectStatementOf returns the inner SelectStatement of a CTE body,
// or nil for non-SELECT bodies (writable CTEs like `WITH x AS (INSERT
// ... RETURNING ...) ...`). Callers fall back to unknownSource for
// the nil case.
func selectStatementOf(stmt tree.Statement) tree.SelectStatement {
	if sel, ok := stmt.(*tree.Select); ok && sel != nil {
		return sel.Select
	}
	return nil
}

// explicitColumnNames returns the names from an AliasClause.Cols list,
// or nil when the list is empty. ColumnDef.Type is recorded only for
// record-returning function aliases and is irrelevant to name
// resolution.
func explicitColumnNames(cols tree.ColumnDefList) []string {
	if len(cols) == 0 {
		return nil
	}
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = string(c.Name)
	}
	return out
}

// inferProjection walks a SelectStatement and returns the list of
// column names its projection exposes — see projectedColumns for the
// nil-vs-empty contract.
//
// Set ops (UNION/INTERSECT/EXCEPT) inherit their left branch's
// projection — SQL requires both branches to expose the same column
// count and the left branch's labels are the user-visible names.
// ParenSelect descends. SelectClause walks each SelectExpr per
// columnNameOf. VALUES returns nil because its columns are
// positional-only (PostgreSQL labels them column1/column2/...; refs
// by name are not supported here).
func inferProjection(s tree.SelectStatement) []string {
	switch t := s.(type) {
	case *tree.SelectClause:
		return projectionOf(t.Exprs)
	case *tree.UnionClause:
		if t.Left == nil {
			return nil
		}
		return inferProjection(t.Left.Select)
	case *tree.ParenSelect:
		if t.Select == nil {
			return nil
		}
		return inferProjection(t.Select.Select)
	}
	return nil
}

// projectionOf converts a SELECT-list into a column-name slice. Any
// SelectExpr whose name cannot be derived (stars, casts, arithmetic,
// unaliased function calls, ...) defeats inference for the whole
// projection and the function returns nil — partial inference would
// mis-report "available_columns" against the source. The caller then
// records an unknownSource so refs against the alias are silently
// skipped.
func projectionOf(exprs tree.SelectExprs) []string {
	if len(exprs) == 0 {
		return nil
	}
	out := make([]string, 0, len(exprs))
	for _, se := range exprs {
		name, ok := columnNameOf(se)
		if !ok {
			return nil
		}
		out = append(out, name)
	}
	return out
}

// columnNameOf extracts the addressable name for one SelectExpr.
// ok=false signals "the entire projection should be treated as
// unknown" — triggered by stars OR by any expression shape whose
// column name we cannot derive without re-implementing PostgreSQL's
// label-inference rules (cast, type-annotation, arithmetic,
// indirection, function calls without aliases, ...). Defeating
// inference is the conservative choice: pretending an opaque
// expression contributes no column would mis-report
// available_columns and false-positive a valid ref like
// `SELECT t.id FROM (SELECT id::int FROM users) t`.
//
// Bare column references parse as *tree.UnresolvedName (with
// Parts[0] = column, Parts[1..] = qualifiers) regardless of how
// many parts the user wrote — *tree.ColumnItem is produced only
// after typecheck, which semcheck does not run, so it is not
// handled here.
func columnNameOf(se tree.SelectExpr) (string, bool) {
	if se.As != "" {
		return string(se.As), true
	}
	if u, ok := se.Expr.(*tree.UnresolvedName); ok && !u.Star {
		return u.Parts[0], true
	}
	return "", false
}

// containsFold reports whether names contains target under ASCII
// case folding. Catalog column names are stored as authored, so a
// case-insensitive compare is needed for refs like "SELECT ID FROM
// users" against a column declared lowercase.
func containsFold(names []string, target string) bool {
	for _, n := range names {
		if strings.EqualFold(n, target) {
			return true
		}
	}
	return false
}
