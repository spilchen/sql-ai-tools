// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package risk

import (
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/tree"
)

// isStar returns true if expr represents a star expression — either an
// unqualified star (SELECT *), a qualified star (SELECT t.*), or an
// all-columns selector. The parser represents these as
// tree.UnqualifiedStar, *tree.UnresolvedName with Star=true, and
// *tree.AllColumnsSelector, respectively.
func isStar(expr tree.Expr) bool {
	switch e := expr.(type) {
	case tree.UnqualifiedStar:
		return true
	case *tree.UnresolvedName:
		return e.Star
	case *tree.AllColumnsSelector:
		return true
	}
	return false
}

// DefaultRules returns the built-in set of AST-only risk rules.
func DefaultRules() []Rule {
	return []Rule{
		deleteNoWhereRule,
		updateNoWhereRule,
		dropTableRule,
		selectStarRule,
	}
}

// deleteNoWhereRule flags DELETE statements that have neither a WHERE
// clause nor a LIMIT clause, matching CockroachDB's sql_safe_updates
// behavior.
var deleteNoWhereRule = Rule{
	ReasonCode: "DELETE_NO_WHERE",
	Severity:   SeverityCritical,
	Category:   "data_safety",
	Check: func(input CheckInput) []Finding {
		del, ok := input.AST.(*tree.Delete)
		if !ok {
			return nil
		}
		if del.Where != nil || del.Limit != nil {
			return nil
		}
		return []Finding{{
			Message:  "DELETE without WHERE clause affects all rows",
			Position: &input.Position,
			FixHint:  "Add a WHERE clause to limit affected rows",
		}}
	},
}

// updateNoWhereRule flags UPDATE statements that have neither a WHERE
// clause nor a LIMIT clause, matching CockroachDB's sql_safe_updates
// behavior.
var updateNoWhereRule = Rule{
	ReasonCode: "UPDATE_NO_WHERE",
	Severity:   SeverityCritical,
	Category:   "data_safety",
	Check: func(input CheckInput) []Finding {
		upd, ok := input.AST.(*tree.Update)
		if !ok {
			return nil
		}
		if upd.Where != nil || upd.Limit != nil {
			return nil
		}
		return []Finding{{
			Message:  "UPDATE without WHERE clause affects all rows",
			Position: &input.Position,
			FixHint:  "Add a WHERE clause to limit affected rows",
		}}
	},
}

// dropTableRule flags all DROP TABLE statements. The message includes
// the table name(s) being dropped.
var dropTableRule = Rule{
	ReasonCode: "DROP_TABLE",
	Severity:   SeverityCritical,
	Category:   "data_safety",
	Check: func(input CheckInput) []Finding {
		dt, ok := input.AST.(*tree.DropTable)
		if !ok {
			return nil
		}
		names := make([]string, len(dt.Names))
		for i, n := range dt.Names {
			names[i] = n.String()
		}
		return []Finding{{
			Message:  fmt.Sprintf("DROP TABLE %s permanently removes the table and all its data", strings.Join(names, ", ")),
			Position: &input.Position,
			FixHint:  "Verify the table name and consider backing up data first",
		}}
	},
}

// selectStarRule flags SELECT statements that use an unqualified star
// (SELECT *) or a qualified star (SELECT t.*). This is a low-severity
// hint: star selects can cause unexpected column additions to propagate
// and make queries fragile.
var selectStarRule = Rule{
	ReasonCode: "SELECT_STAR",
	Severity:   SeverityLow,
	Category:   "performance",
	Check: func(input CheckInput) []Finding {
		sel, ok := input.AST.(*tree.Select)
		if !ok {
			return nil
		}
		sc, ok := sel.Select.(*tree.SelectClause)
		if !ok {
			return nil
		}
		for _, expr := range sc.Exprs {
			if isStar(expr.Expr) {
				return []Finding{{
					Message:  "SELECT * returns all columns and may cause performance issues",
					Position: &input.Position,
					FixHint:  "List specific columns instead of using *",
				}}
			}
		}
		return nil
	},
}
