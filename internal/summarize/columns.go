// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package summarize

import (
	"strings"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/tree"
)

// collectReferences returns every column the statement reads or
// writes, deduplicated case-insensitively in first-seen order. Refs
// are emitted as the user wrote them: bare "col" when unqualified,
// "qualifier.col" when the source SQL named a table prefix. We do not
// resolve bare names against table schemas — there is no catalog
// here, so a bare "id" in a multi-table FROM stays bare.
//
// Mutated targets stored as tree.NameList (INSERT explicit column
// list at Insert.Columns, UPDATE SET LHS at UpdateExpr.Names, ON
// CONFLICT arbiter cols at OnConflict.Columns, JOIN USING cols at
// UsingJoinCond.Cols) are not Expr nodes, so the visitor cannot
// reach them. INSERT and UPDATE LHS sets are unioned in by
// summarizeStatement via mergeColumns(refs, AffectedColumns) so the
// "ReferencedColumns ⊇ AffectedColumns" invariant holds for those
// cases. ON CONFLICT arbiter cols and DO UPDATE SET LHS are added
// by addOnConflictNameLists below because no Expr walker reaches
// them. JOIN USING cols remain unsurfaced — documented in the test
// expectations.
//
// Star projections (SELECT *, t.*) are deliberately skipped — see
// hasProjectionStar for the separate flag agents use to detect that
// the column list is incomplete.
func collectReferences(stmt tree.Statement) []string {
	v := newReferenceCollector()
	tree.WalkStmt(v, stmt)
	if ins, ok := stmt.(*tree.Insert); ok && ins.OnConflict != nil {
		v.addOnConflictNameLists(ins.OnConflict)
	}
	return v.ordered
}

// hasProjectionStar reports whether the statement's outermost
// projection list contains a bare "*" or qualified "t.*". Function
// arguments like count(*) are NOT counted: they don't introduce an
// unenumerated column footprint.
//
// "Outermost" includes the branches of a top-level set operation
// (UNION/INTERSECT/EXCEPT) — both sides contribute to the result
// shape. Stars deeper inside a CTE body or scalar subquery do not
// set this flag; the outer query still names its own columns
// explicitly (or is itself a star, which would fire the check at
// its own level). Note: this leaves a star-driven gap when an outer
// query selects from a CTE that itself uses SELECT * — the outer
// projection looks complete but its source is incomplete. Agents
// needing that level of catalog-free incompleteness detection will
// need a future enhancement.
//
// For INSERT ... SELECT we look at the embedded SELECT's projection
// because that is what determines the source-row column shape; for
// UPDATE/DELETE/OTHER, false (those statements have no projection).
func hasProjectionStar(stmt tree.Statement) bool {
	switch n := stmt.(type) {
	case *tree.Select:
		return selectStatementHasStar(n.Select)
	case *tree.Insert:
		if n.Rows != nil {
			return selectStatementHasStar(n.Rows.Select)
		}
	}
	return false
}

// selectStatementHasStar inspects one SelectStatement node. It
// recurses into UnionClause branches so a top-level set operation
// where either side projects "*" still flips the flag. VALUES
// clauses and unrecognized SelectStatement shapes return false: a
// VALUES literal cannot be a star, and unknown shapes are treated
// as "no detectable star" rather than panicking.
func selectStatementHasStar(s tree.SelectStatement) bool {
	switch n := s.(type) {
	case *tree.SelectClause:
		for _, e := range n.Exprs {
			if isStarExpr(e.Expr) {
				return true
			}
		}
		return false
	case *tree.UnionClause:
		if n.Left != nil && selectStatementHasStar(n.Left.Select) {
			return true
		}
		if n.Right != nil && selectStatementHasStar(n.Right.Select) {
			return true
		}
	}
	return false
}

// isStarExpr identifies the AST shapes the parser produces for a
// star token in projection position:
//   - tree.UnqualifiedStar — bare "*"
//   - *tree.AllColumnsSelector — "t.*" after name resolution
//   - *tree.UnresolvedName with Star=true — "t.*" before name
//     resolution, which is the form the raw parser produces and
//     thus the form summarize sees in practice (summarize never
//     runs name resolution)
func isStarExpr(e tree.Expr) bool {
	switch n := e.(type) {
	case tree.UnqualifiedStar:
		return true
	case *tree.AllColumnsSelector:
		return true
	case *tree.UnresolvedName:
		return n.Star
	}
	return false
}

// referenceCollector implements tree.ExtendedVisitor to gather column
// references from every Expr-typed position in a statement: SELECT
// projection, WHERE, JOIN ON, GROUP BY, HAVING, ORDER BY, RETURNING,
// and any nested subquery or CTE bodies. ExtendedVisitor (rather
// than plain Visitor) is required so WalkStmt descends into
// TableExpr subtrees where JOIN ON conditions live.
//
// Positions stored as tree.NameList — JOIN USING (col), INSERT
// (col, col), UPDATE SET col=…'s LHS, ON CONFLICT (col) — are not
// Expr nodes and the visitor cannot reach them. See collectReferences
// for how the affected→referenced merge and the manual OnConflict
// walk fill those gaps.
//
// Lifecycle: one collector per top-level statement. seen dedupes by
// case-folded "qualifier.name" key so "T.ID" and "t.id" collapse,
// with the first-seen casing winning in the ordered output. ordered
// preserves first-seen AST-walk order for stable JSON.
type referenceCollector struct {
	seen    map[string]struct{}
	ordered []string
}

func newReferenceCollector() *referenceCollector {
	return &referenceCollector{
		seen:    make(map[string]struct{}),
		ordered: []string{},
	}
}

var _ tree.ExtendedVisitor = (*referenceCollector)(nil)

func (v *referenceCollector) VisitPost(expr tree.Expr) tree.Expr                 { return expr }
func (v *referenceCollector) VisitTablePost(e tree.TableExpr) tree.TableExpr     { return e }
func (v *referenceCollector) VisitStatementPost(s tree.Statement) tree.Statement { return s }
func (v *referenceCollector) VisitStatementPre(s tree.Statement) (bool, tree.Statement) {
	return true, s
}
func (v *referenceCollector) VisitTablePre(e tree.TableExpr) (bool, tree.TableExpr) {
	return true, e
}

// VisitPre dispatches on Expr subtype. Leaf cases (UnresolvedName,
// ColumnItem) collect a ref and return recurse=false because there
// is nothing inside a column reference to walk. Star cases collect
// nothing — SelectStar is handled separately by hasProjectionStar —
// and likewise stop. The default returns recurse=true so the walker
// descends into composite expressions (BinaryExpr, FuncExpr,
// ComparisonExpr, CaseExpr, Tuple, Subquery, …). If a new
// column-bearing AST shape ever needs to be recognized (e.g. tree
// rewrites that emit *tree.IndexedVar — currently never reached
// because summarize consumes only freshly parsed input), it must be
// added as an explicit case here or it will be silently missed.
func (v *referenceCollector) VisitPre(expr tree.Expr) (bool, tree.Expr) {
	switch n := expr.(type) {
	case *tree.UnresolvedName:
		// Star is a column-list shorthand, not a column reference.
		if n.Star {
			return false, expr
		}
		v.add(refFromUnresolved(n))
		return false, expr
	case *tree.ColumnItem:
		v.add(refFromColumnItem(n))
		return false, expr
	case tree.UnqualifiedStar, *tree.AllColumnsSelector:
		return false, expr
	}
	return true, expr
}

// addOnConflictNameLists adds the column names that live inside an
// ON CONFLICT clause as NameList values (not Expr nodes), so the
// expression walker cannot reach them. Specifically: the arbiter
// list "ON CONFLICT (col, col)" and each "DO UPDATE SET col=..."
// LHS. The Expr-typed parts (arbiter predicate, SET RHS, UPDATE
// WHERE) are walked by the upstream parser when it sees an
// ExtendedVisitor; they don't need help here.
func (v *referenceCollector) addOnConflictNameLists(oc *tree.OnConflict) {
	for _, n := range oc.Columns {
		v.add("", string(n))
	}
	for _, e := range oc.Exprs {
		// Defensive: the parser does not emit nil entries today.
		if e == nil {
			continue
		}
		for _, n := range e.Names {
			v.add("", string(n))
		}
	}
}

// add records a (qualifier, name) ref, deduplicating
// case-insensitively. The empty-name guard is defensive: every
// caller is expected to filter unnamed inputs upstream
// (refFromUnresolved guards on n.Star, VisitPre guards on
// UnresolvedName.Star), so reaching this branch with name=="" would
// indicate an unexpected AST shape and is silently ignored rather
// than panicking.
func (v *referenceCollector) add(qualifier, name string) {
	if name == "" {
		return
	}
	rendered := name
	if qualifier != "" {
		rendered = qualifier + "." + name
	}
	key := strings.ToLower(rendered)
	if _, dup := v.seen[key]; dup {
		return
	}
	v.seen[key] = struct{}{}
	v.ordered = append(v.ordered, rendered)
}

// refFromUnresolved extracts (qualifier, name) from an UnresolvedName.
// UnresolvedName.Parts is stored in reverse order (column, table,
// schema, catalog), so Parts[0] is always the column and Parts[1],
// when present, is the table-or-alias qualifier. Schema/catalog
// prefixes (Parts[2], Parts[3]) are dropped — they don't help an
// agent reasoning about column footprints.
//
// The Star branch returns ("","") so VisitPre's pre-filter has a
// safe fallback if the caller order ever changes; callers should
// still filter on n.Star themselves so the dispatch reads cleanly.
//
// Mirrors the helper in internal/semcheck/names.go; duplicated
// rather than imported to keep summarize free of a semcheck
// dependency.
func refFromUnresolved(n *tree.UnresolvedName) (qualifier, name string) {
	if n.Star {
		return "", ""
	}
	name = n.Parts[0]
	if n.NumParts >= 2 {
		qualifier = n.Parts[1]
	}
	return qualifier, name
}

// refFromColumnItem extracts (qualifier, name) from a ColumnItem.
// Most refs surface as UnresolvedName because summarize consumes
// raw parser output without running name resolution; ColumnItem
// appears after tree.NormalizeColumnItem and is handled here only
// as a defensive case so callers don't have to special-case it if
// summarize ever starts accepting normalized input.
func refFromColumnItem(c *tree.ColumnItem) (qualifier, name string) {
	name = string(c.ColumnName)
	if c.TableName != nil && c.TableName.NumParts >= 1 {
		qualifier = c.TableName.Parts[0]
	}
	return qualifier, name
}
