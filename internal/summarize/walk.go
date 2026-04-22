// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package summarize

import (
	"strings"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/tree"
)

// collectTables returns the bare names of every table the statement
// reads or writes, deduplicated case-insensitively. Order matches the
// parser's AST traversal of the statement — for SelectClause that
// means projection, then various clauses (WHERE, GROUP BY, HAVING,
// …), then FROM tables — so a table referenced inside a WHERE or
// GROUP BY subquery appears before the outer FROM table. The order
// is stable for a given input but should be treated by consumers as
// unordered. CTE names declared by WITH clauses and table aliases are
// excluded; numeric "[123 AS t]" table refs are skipped because they
// have no resolvable name.
func collectTables(stmt tree.Statement) []string {
	v := newTableCollector()
	bindCTEs(v, stmt)
	tree.WalkStmt(v, stmt)
	if len(v.ordered) == 0 {
		return []string{}
	}
	return v.ordered
}

// collectJoins walks stmt and returns one Join entry per JOIN clause
// found anywhere in the statement (including inside subqueries).
// Order is depth-first post-order, which for left-deep join chains
// (`a JOIN b JOIN c`) matches the lexical order: inner join first,
// outer join last.
func collectJoins(stmt tree.Statement) []Join {
	v := &joinCollector{}
	tree.WalkStmt(v, stmt)
	if len(v.joins) == 0 {
		return []Join{}
	}
	return v.joins
}

// bindCTEs preloads CTE alias names into the table collector's scope
// so references like `SELECT * FROM x` (where x is a CTE alias) do
// not appear in the tables list. We mirror the per-CTE walk that
// internal/semcheck/names.go does for catalog resolution.
func bindCTEs(v *tableCollector, stmt tree.Statement) {
	w := withClause(stmt)
	if w == nil {
		return
	}
	for _, cte := range w.CTEList {
		if cte == nil {
			continue
		}
		if name := string(cte.Name.Alias); name != "" {
			v.scope[strings.ToLower(name)] = struct{}{}
		}
	}
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

// tableCollector implements tree.ExtendedVisitor to gather table
// names. ExtendedVisitor (rather than plain Visitor) is required
// because WalkStmt with a plain Visitor skips TableExpr subtrees,
// which is exactly where table references live.
//
// Lifecycle: one collector per top-level statement. The seen map
// dedupes case-insensitively; ordered preserves first-seen order so
// JSON output is stable and matches what a human reading the SQL
// would expect.
type tableCollector struct {
	seen    map[string]struct{}
	scope   map[string]struct{}
	ordered []string
}

func newTableCollector() *tableCollector {
	return &tableCollector{
		seen:  make(map[string]struct{}),
		scope: make(map[string]struct{}),
	}
}

var _ tree.ExtendedVisitor = (*tableCollector)(nil)

func (v *tableCollector) VisitPre(expr tree.Expr) (bool, tree.Expr)          { return true, expr }
func (v *tableCollector) VisitPost(expr tree.Expr) tree.Expr                 { return expr }
func (v *tableCollector) VisitTablePost(e tree.TableExpr) tree.TableExpr     { return e }
func (v *tableCollector) VisitStatementPost(s tree.Statement) tree.Statement { return s }

func (v *tableCollector) VisitStatementPre(stmt tree.Statement) (bool, tree.Statement) {
	// Subqueries can introduce their own CTEs; bind them before the
	// body is walked so nested references don't leak into tables.
	bindCTEs(v, stmt)
	return true, stmt
}

func (v *tableCollector) VisitTablePre(expr tree.TableExpr) (bool, tree.TableExpr) {
	switch t := expr.(type) {
	case *tree.AliasedTableExpr:
		if t.As.Alias != "" {
			v.scope[strings.ToLower(string(t.As.Alias))] = struct{}{}
		}
		return true, expr
	case *tree.TableName:
		v.add(t.Table())
		return false, expr
	case *tree.TableRef:
		// Numeric "[123 AS t]" form has no resolvable name.
		return false, expr
	}
	return true, expr
}

func (v *tableCollector) add(name string) {
	if name == "" {
		return
	}
	key := strings.ToLower(name)
	if _, dup := v.seen[key]; dup {
		return
	}
	if _, scoped := v.scope[key]; scoped {
		return
	}
	v.seen[key] = struct{}{}
	v.ordered = append(v.ordered, name)
}

// joinCollector gathers JoinTableExpr nodes via ExtendedVisitor. We
// recurse rather than stop at JOIN nodes so that nested joins
// (e.g. `a JOIN b ON ... JOIN c ON ...`) all surface as separate
// Join entries.
type joinCollector struct {
	joins []Join
}

var _ tree.ExtendedVisitor = (*joinCollector)(nil)

func (v *joinCollector) VisitPre(expr tree.Expr) (bool, tree.Expr) { return true, expr }
func (v *joinCollector) VisitPost(expr tree.Expr) tree.Expr        { return expr }
func (v *joinCollector) VisitStatementPre(s tree.Statement) (bool, tree.Statement) {
	return true, s
}
func (v *joinCollector) VisitStatementPost(s tree.Statement) tree.Statement { return s }
func (v *joinCollector) VisitTablePre(expr tree.TableExpr) (bool, tree.TableExpr) {
	return true, expr
}

// VisitTablePost records the join after children are visited so a
// left-deep chain like `a JOIN b ON ... JOIN c ON ...` produces
// [(a,b), (_,c)] rather than [(_,c), (a,b)] — i.e. the order a human
// reader would expect.
func (v *joinCollector) VisitTablePost(expr tree.TableExpr) tree.TableExpr {
	if j, ok := expr.(*tree.JoinTableExpr); ok {
		v.joins = append(v.joins, joinFrom(j))
	}
	return expr
}

// joinFrom builds a Join from a parsed JoinTableExpr.
//
// Type combines the parser's JoinType (INNER/LEFT/RIGHT/FULL/CROSS)
// with the natural-join marker. A NATURAL INNER JOIN renders as
// "NATURAL", a NATURAL LEFT JOIN as "NATURAL LEFT", etc. — so an
// agent reading just Type still sees that no equality predicate was
// declared, instead of the join silently looking like a regular
// INNER. The parser leaves JoinType empty for the default INNER, which
// we promote to "INNER" so the JSON output never has a blank type.
func joinFrom(j *tree.JoinTableExpr) Join {
	jt := j.JoinType
	if jt == "" {
		jt = tree.AstInner
	}
	if _, isNatural := j.Cond.(tree.NaturalJoinCond); isNatural {
		if jt == tree.AstInner {
			jt = "NATURAL"
		} else {
			jt = "NATURAL " + jt
		}
	}
	return Join{
		Type:      jt,
		Left:      tableNameOf(j.Left),
		Right:     tableNameOf(j.Right),
		Condition: joinCondString(j.Cond),
	}
}

// tableNameOf returns the bare table name (or alias) of a join side
// when it's a simple table reference, and "" for nested joins or
// subqueries — the renderer would otherwise emit a multi-line
// expression that's not useful as an identifier.
func tableNameOf(t tree.TableExpr) string {
	switch e := t.(type) {
	case *tree.AliasedTableExpr:
		if e.As.Alias != "" {
			return string(e.As.Alias)
		}
		return tableNameOf(e.Expr)
	case *tree.TableName:
		return e.Table()
	case *tree.ParenTableExpr:
		return tableNameOf(e.Expr)
	}
	return ""
}

// joinCondString renders a JoinCond into the conventional SQL form.
// Returns "" for a nil cond (CROSS JOIN), so JSON omitempty drops the
// field entirely for those joins.
func joinCondString(c tree.JoinCond) string {
	switch cond := c.(type) {
	case nil:
		return ""
	case tree.NaturalJoinCond:
		return "NATURAL"
	case *tree.OnJoinCond:
		return tree.AsStringWithFlags(cond.Expr, tree.FmtSimple)
	case *tree.UsingJoinCond:
		return "USING (" + tree.AsStringWithFlags(&cond.Cols, tree.FmtSimple) + ")"
	}
	return ""
}
