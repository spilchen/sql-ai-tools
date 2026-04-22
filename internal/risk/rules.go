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
		dropDatabaseRule,
		alterTableDropColumnRule,
		selectForUpdateNoWhereRule,
		selectForShareNoWhereRule,
		selectStarRule,
	}
}

// selectHasLockingStrength reports whether sel carries a locking clause
// with the given strength (e.g. FOR UPDATE, FOR SHARE).
func selectHasLockingStrength(sel *tree.Select, strength tree.LockingStrength) bool {
	for _, item := range sel.Locking {
		if item != nil && item.Strength == strength {
			return true
		}
	}
	return false
}

// selectHasWhere reports whether the inner SelectClause of sel has a
// WHERE clause. For non-SelectClause inner statements (UNION, VALUES,
// ParenSelect, etc.) it conservatively returns false even though such
// shapes can carry a meaningful predicate, e.g. `(SELECT id FROM t
// WHERE id=1) FOR UPDATE`. This means the FOR UPDATE / FOR SHARE rules
// will flag those as missing-WHERE — a deliberate trade-off in favor
// of a simple, AST-shape-only check; recursing into inner selects is
// future work if the false-positive rate becomes a problem.
func selectHasWhere(sel *tree.Select) bool {
	sc, ok := sel.Select.(*tree.SelectClause)
	if !ok {
		return false
	}
	return sc.Where != nil
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

// dropDatabaseRule flags all DROP DATABASE statements. DROP DATABASE
// is irreversible and cascades to every schema, table, and row the
// database contains, so it is always treated as critical regardless of
// IF EXISTS or DROP behavior modifiers.
var dropDatabaseRule = Rule{
	ReasonCode: "DROP_DATABASE",
	Severity:   SeverityCritical,
	Category:   "data_safety",
	Check: func(input CheckInput) []Finding {
		dd, ok := input.AST.(*tree.DropDatabase)
		if !ok {
			return nil
		}
		return []Finding{{
			Message:  fmt.Sprintf("DROP DATABASE %s permanently removes the database and all its objects", dd.Name.String()),
			Position: &input.Position,
			FixHint:  "Verify the database name and consider backing up data first",
		}}
	},
}

// alterTableDropColumnRule flags every DROP COLUMN command inside an
// ALTER TABLE statement. A single ALTER TABLE may carry multiple
// commands (e.g. `ALTER TABLE t DROP COLUMN a, DROP COLUMN b`); each
// drop produces its own finding so callers can act on them
// independently.
var alterTableDropColumnRule = Rule{
	ReasonCode: "ALTER_TABLE_DROP_COLUMN",
	Severity:   SeverityHigh,
	Category:   "schema_safety",
	Check: func(input CheckInput) []Finding {
		at, ok := input.AST.(*tree.AlterTable)
		if !ok {
			return nil
		}
		var findings []Finding
		table := at.Table.String()
		for _, cmd := range at.Cmds {
			drop, ok := cmd.(*tree.AlterTableDropColumn)
			if !ok {
				continue
			}
			findings = append(findings, Finding{
				Message:  fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s permanently removes the column and its data", table, drop.Column.String()),
				Position: &input.Position,
				FixHint:  "Confirm no application reads or writes this column before dropping",
			})
		}
		return findings
	},
}

// selectForUpdateNoWhereRule flags `SELECT ... FOR UPDATE` queries that
// have neither a WHERE clause nor a LIMIT. Without a predicate, the
// statement takes a write lock on every row in the table, matching
// CockroachDB's sql_safe_updates behavior.
var selectForUpdateNoWhereRule = Rule{
	ReasonCode: "SELECT_FOR_UPDATE_NO_WHERE",
	Severity:   SeverityCritical,
	Category:   "data_safety",
	Check: func(input CheckInput) []Finding {
		sel, ok := input.AST.(*tree.Select)
		if !ok {
			return nil
		}
		if !selectHasLockingStrength(sel, tree.ForUpdate) {
			return nil
		}
		if sel.Limit != nil || selectHasWhere(sel) {
			return nil
		}
		return []Finding{{
			Message:  "SELECT ... FOR UPDATE without WHERE or LIMIT locks every row in the table",
			Position: &input.Position,
			FixHint:  "Add a WHERE clause or LIMIT to scope the lock",
		}}
	},
}

// selectForShareNoWhereRule flags `SELECT ... FOR SHARE` queries that
// have neither a WHERE clause nor a LIMIT. Like FOR UPDATE, a missing
// predicate causes the lock to span every row in the table.
var selectForShareNoWhereRule = Rule{
	ReasonCode: "SELECT_FOR_SHARE_NO_WHERE",
	Severity:   SeverityHigh,
	Category:   "data_safety",
	Check: func(input CheckInput) []Finding {
		sel, ok := input.AST.(*tree.Select)
		if !ok {
			return nil
		}
		if !selectHasLockingStrength(sel, tree.ForShare) {
			return nil
		}
		if sel.Limit != nil || selectHasWhere(sel) {
			return nil
		}
		return []Finding{{
			Message:  "SELECT ... FOR SHARE without WHERE or LIMIT locks every row in the table",
			Position: &input.Position,
			FixHint:  "Add a WHERE clause or LIMIT to scope the lock",
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
