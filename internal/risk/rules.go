// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package risk

import (
	"fmt"
	"go/constant"
	"strings"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser/statements"
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

// DefaultRules returns the built-in set of AST-only risk rules that
// inspect a single statement at a time.
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
		truncateTableRule,
		serialPrimaryKeyRule,
		missingPrimaryKeyRule,
		largeOffsetRule,
		xaPreparedTxnRule,
		savepointCockroachRestartRule,
	}
}

// DefaultMultiRules returns the built-in set of AST-only risk rules
// that need to look across more than one parsed statement at a time.
func DefaultMultiRules() []MultiRule {
	return []MultiRule{
		multipleDDLInTxnRule,
		ddlAndDMLInTxnRule,
	}
}

// largeOffsetThreshold is the OFFSET literal at or above which the
// largeOffsetRule fires. Offset pagination is O(offset) on
// CockroachDB — every skipped row is still scanned — so deep offsets
// turn into hidden full-table scans. 1000 is intentionally high to
// avoid noise on routine admin or one-off queries; keyset pagination
// is preferable well below this threshold once row width or per-row
// work grows.
const largeOffsetThreshold = 1000

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

// truncateTableRule flags every TRUNCATE TABLE statement. TRUNCATE
// removes every row in the named table(s) unconditionally — there is
// no WHERE clause to scope it — so it is always treated as critical
// regardless of how it is invoked. CASCADE additionally removes rows
// in dependent tables via foreign keys; the message calls that out so
// callers can see the wider blast radius.
var truncateTableRule = Rule{
	ReasonCode: "TRUNCATE_TABLE",
	Severity:   SeverityCritical,
	Category:   "data_safety",
	Check: func(input CheckInput) []Finding {
		tr, ok := input.AST.(*tree.Truncate)
		if !ok {
			return nil
		}
		names := make([]string, len(tr.Tables))
		for i, n := range tr.Tables {
			names[i] = n.String()
		}
		msg := fmt.Sprintf("TRUNCATE TABLE %s permanently removes every row in the table", strings.Join(names, ", "))
		hint := "Verify the table name; use DELETE with a WHERE clause if you only need to remove some rows"
		if tr.DropBehavior == tree.DropCascade {
			msg = fmt.Sprintf("TRUNCATE TABLE %s CASCADE permanently removes every row in the table and all rows in tables referencing it via foreign keys", strings.Join(names, ", "))
			hint = "CASCADE here also wipes dependent tables — confirm the full set of affected tables before running"
		}
		return []Finding{{
			Message:  msg,
			Position: &input.Position,
			FixHint:  hint,
		}}
	},
}

// serialPrimaryKeyRule flags CREATE TABLE statements where the primary
// key column is declared with the SERIAL family (SERIAL, SMALLSERIAL,
// BIGSERIAL, SERIAL2/4/8). In a distributed database these create
// monotonically increasing values that all land on the same range,
// producing a write hotspot. Recommended replacements are
// `UUID PRIMARY KEY DEFAULT gen_random_uuid()` for most tables, or
// `INT8 GENERATED BY DEFAULT AS IDENTITY` when a numeric key is required.
var serialPrimaryKeyRule = Rule{
	ReasonCode: "SERIAL_PRIMARY_KEY",
	Severity:   SeverityHigh,
	Category:   "performance",
	Check: func(input CheckInput) []Finding {
		ct, ok := input.AST.(*tree.CreateTable)
		if !ok {
			return nil
		}
		// Build the set of column names that participate in the
		// primary key, whether declared inline (PRIMARY KEY on the
		// column) or via a table-level UNIQUE...PRIMARY KEY constraint.
		pkCols := make(map[tree.Name]bool)
		for _, def := range ct.Defs {
			switch d := def.(type) {
			case *tree.ColumnTableDef:
				if d.PrimaryKey.IsPrimaryKey {
					pkCols[d.Name] = true
				}
			case *tree.UniqueConstraintTableDef:
				if d.PrimaryKey {
					for _, idxCol := range d.Columns {
						pkCols[idxCol.Column] = true
					}
				}
			}
		}
		if len(pkCols) == 0 {
			return nil
		}
		var findings []Finding
		table := ct.Table.String()
		for _, def := range ct.Defs {
			col, ok := def.(*tree.ColumnTableDef)
			if !ok || !col.IsSerial || !pkCols[col.Name] {
				continue
			}
			findings = append(findings, Finding{
				Message:  fmt.Sprintf("CREATE TABLE %s: primary key column %s uses SERIAL, which creates a write hotspot in CockroachDB", table, col.Name),
				Position: &input.Position,
				FixHint:  "Use UUID PRIMARY KEY DEFAULT gen_random_uuid() or INT8 GENERATED BY DEFAULT AS IDENTITY instead",
			})
		}
		return findings
	},
}

// missingPrimaryKeyRule flags CREATE TABLE statements that do not
// declare any primary key. CockroachDB silently adds a hidden
// `rowid` column in this case, which is a sequential integer — the
// same hotspot pattern as SERIAL — and which the application cannot
// reference. CREATE TABLE AS (`ct.AsSource != nil`) is exempt because
// the column list is derived from the source query and the user has no
// place to declare a key inline.
var missingPrimaryKeyRule = Rule{
	ReasonCode: "MISSING_PRIMARY_KEY",
	Severity:   SeverityMedium,
	Category:   "schema_safety",
	Check: func(input CheckInput) []Finding {
		ct, ok := input.AST.(*tree.CreateTable)
		if !ok || ct.AsSource != nil {
			return nil
		}
		for _, def := range ct.Defs {
			switch d := def.(type) {
			case *tree.ColumnTableDef:
				if d.PrimaryKey.IsPrimaryKey {
					return nil
				}
			case *tree.UniqueConstraintTableDef:
				if d.PrimaryKey {
					return nil
				}
			}
		}
		return []Finding{{
			Message:  fmt.Sprintf("CREATE TABLE %s has no PRIMARY KEY; CockroachDB will add a hidden sequential rowid column that becomes a write hotspot", ct.Table.String()),
			Position: &input.Position,
			FixHint:  "Declare an explicit PRIMARY KEY, preferably UUID DEFAULT gen_random_uuid()",
		}}
	},
}

// largeOffsetRule flags SELECT statements whose OFFSET is a numeric
// literal at or above largeOffsetThreshold. CockroachDB has to scan
// and discard every row up to the offset, so deep offset pagination
// silently turns into a full-table scan. The rule fires for any
// numeric literal — integer (`OFFSET 5000`), float (`OFFSET 5e3`),
// or value that overflows int64 — but not for non-literal offsets
// like `OFFSET (SELECT ...)` or parameter placeholders, where the
// value is unknown at parse time.
var largeOffsetRule = Rule{
	ReasonCode: "LARGE_OFFSET",
	Severity:   SeverityMedium,
	Category:   "performance",
	Check: func(input CheckInput) []Finding {
		sel, ok := input.AST.(*tree.Select)
		if !ok || sel.Limit == nil || sel.Limit.Offset == nil {
			return nil
		}
		num, ok := sel.Limit.Offset.(*tree.NumVal)
		if !ok {
			return nil
		}
		// Compare via float so that fractional and scientific-notation
		// literals are evaluated, and so that values that overflow
		// int64 still fire. Float64Val maps any literal above
		// largeOffsetThreshold to a value at least as large (a finite
		// float for most int64-overflowing literals, +Inf only for
		// literals beyond float64's range), so this comparison is
		// safe regardless of magnitude.
		val := num.AsConstantValue()
		if val == nil {
			return nil
		}
		f, _ := constant.Float64Val(constant.ToFloat(val))
		if f < float64(largeOffsetThreshold) {
			return nil
		}
		return []Finding{{
			Message:  fmt.Sprintf("OFFSET %s scans and discards every preceding row, which degrades to a full-table scan as the offset grows", num.String()),
			Position: &input.Position,
			FixHint:  "Use keyset pagination (WHERE (sort_col, id) < ($last_sort, $last_id) ORDER BY ... LIMIT N) instead",
		}}
	},
}

// xaPreparedTxnRule flags PostgreSQL XA / two-phase-commit statements
// (`PREPARE TRANSACTION`, `COMMIT PREPARED`, `ROLLBACK PREPARED`).
// These detach the transaction from the session: the prepared state
// outlives the connection, holding row locks until an external
// coordinator issues a matching COMMIT/ROLLBACK PREPARED. A forgotten
// prepared transaction silently blocks every future writer to the
// locked rows. Use the outbox pattern (write business data + outbox
// row in one CockroachDB txn, then deliver via changefeed) for
// cross-system consistency instead.
var xaPreparedTxnRule = Rule{
	ReasonCode: "XA_PREPARED_TXN",
	Severity:   SeverityHigh,
	Category:   "transactional_safety",
	Check: func(input CheckInput) []Finding {
		var verb string
		switch input.AST.(type) {
		case *tree.PrepareTransaction:
			verb = "PREPARE TRANSACTION"
		case *tree.CommitPrepared:
			verb = "COMMIT PREPARED"
		case *tree.RollbackPrepared:
			verb = "ROLLBACK PREPARED"
		default:
			return nil
		}
		return []Finding{{
			Message:  fmt.Sprintf("%s is a two-phase-commit (XA) statement; the prepared transaction state outlives the session and holds row locks until an external coordinator commits or rolls it back", verb),
			Position: &input.Position,
			FixHint:  "For cross-system consistency use the outbox pattern: write business data and an outbox row in one CockroachDB transaction, then deliver via a changefeed",
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

// cockroachRestartSavepointName is the well-known savepoint name used
// by the legacy CockroachDB client-side retry pattern.
const cockroachRestartSavepointName = "cockroach_restart"

// savepointName extracts the savepoint name from a SAVEPOINT, RELEASE
// SAVEPOINT, or ROLLBACK TO SAVEPOINT statement and reports whether
// the AST was one of those statement kinds.
func savepointName(stmt tree.Statement) (tree.Name, bool) {
	switch s := stmt.(type) {
	case *tree.Savepoint:
		return s.Name, true
	case *tree.ReleaseSavepoint:
		return s.Savepoint, true
	case *tree.RollbackToSavepoint:
		return s.Savepoint, true
	}
	return "", false
}

// savepointCockroachRestartRule flags any use of the well-known
// `cockroach_restart` savepoint name. This is the legacy client-side
// retry pattern for serialization failures (SQLSTATE 40001). The
// recommended pattern in modern CockroachDB is to retry the whole
// BEGIN..COMMIT with exponential backoff in the application or
// driver, rather than relying on a per-savepoint rollback. The
// savepoint syntax is still accepted, but applications that depend
// on it are running fragile retry logic; the fix is to move retry
// out of SQL and into the caller. Comparison is case-insensitive so
// quoted variants like "Cockroach_Restart" are also caught.
var savepointCockroachRestartRule = Rule{
	ReasonCode: "SAVEPOINT_COCKROACH_RESTART",
	Severity:   SeverityMedium,
	Category:   "transactional_safety",
	Check: func(input CheckInput) []Finding {
		name, ok := savepointName(input.AST)
		if !ok || !strings.EqualFold(string(name), cockroachRestartSavepointName) {
			return nil
		}
		var verb string
		switch input.AST.(type) {
		case *tree.Savepoint:
			verb = "SAVEPOINT cockroach_restart"
		case *tree.ReleaseSavepoint:
			verb = "RELEASE SAVEPOINT cockroach_restart"
		case *tree.RollbackToSavepoint:
			verb = "ROLLBACK TO SAVEPOINT cockroach_restart"
		}
		return []Finding{{
			Message:  fmt.Sprintf("%s is the legacy client-side retry pattern; modern CockroachDB recommends retrying the whole BEGIN..COMMIT in the application instead", verb),
			Position: &input.Position,
			FixHint:  "Drop the savepoint and retry the entire BEGIN..COMMIT block with exponential backoff on SQLSTATE 40001",
		}}
	},
}

// txnBlock identifies one explicit BEGIN..COMMIT/ROLLBACK span
// within a parsed statement list. Start is the index of the BEGIN
// statement; End is the index of the closing COMMIT/ROLLBACK, or
// len(stmts) if the block was never closed. The half-open range
// [Start+1, End) covers the inner statements.
type txnBlock struct {
	Start, End int
}

// explicitTxnBlocks scans stmts for BEGIN..COMMIT/ROLLBACK spans and
// returns one txnBlock per BEGIN encountered. CockroachDB does not
// support real nested transactions, but the parser accepts a BEGIN
// that appears while another block is open; in that case we open a
// new block at the inner BEGIN and close it on the next
// COMMIT/ROLLBACK (LIFO via a stack). Two consequences worth noting:
//
//   - In a nested case, the outer block's [Start+1, End) range
//     subsumes the inner block's range, so each inner statement is
//     visible to multi-rules as a member of every enclosing block.
//     This matches CRDB semantics: anything inside the outer BEGIN is
//     part of that outer transaction.
//   - Any block that never sees a matching COMMIT/ROLLBACK keeps its
//     initial End = len(stmts), so its range extends to end-of-input.
//     Multi-rules then over-flag rather than under-flag, which is the
//     intended conservative stance here.
//
// Surplus COMMIT/ROLLBACK statements with no open block are ignored.
// Statements outside any block (implicit transactions) do not
// contribute to any txnBlock.
func explicitTxnBlocks(stmts statements.Statements) []txnBlock {
	var blocks []txnBlock
	// openStack holds indexes into blocks for currently-open spans;
	// the innermost open block is at the top.
	var openStack []int
	for i, s := range stmts {
		switch s.AST.(type) {
		case *tree.BeginTransaction:
			blocks = append(blocks, txnBlock{Start: i, End: len(stmts)})
			openStack = append(openStack, len(blocks)-1)
		case *tree.CommitTransaction, *tree.RollbackTransaction:
			if len(openStack) == 0 {
				continue
			}
			top := openStack[len(openStack)-1]
			openStack = openStack[:len(openStack)-1]
			blocks[top].End = i
		}
	}
	return blocks
}

// multipleDDLInTxnRule flags any explicit BEGIN..COMMIT block that
// contains more than one DDL statement. CockroachDB runs each DDL as
// an asynchronous online schema change job; bundling several into one
// user transaction is fragile because a failure mid-transaction can
// leave the schema in a partially-applied state, and dropped names
// cannot be reused in the same transaction. Best practice is one DDL
// per transaction. The rule emits one finding per *extra* DDL beyond
// the first, each pointing at the offending DDL's position, so a
// block with three DDLs produces two findings.
var multipleDDLInTxnRule = MultiRule{
	ReasonCode: "MULTIPLE_DDL_IN_TXN",
	Severity:   SeverityHigh,
	Category:   "transactional_safety",
	Check: func(input MultiCheckInput) []Finding {
		var findings []Finding
		for _, blk := range explicitTxnBlocks(input.Stmts) {
			seenFirst := false
			for i := blk.Start + 1; i < blk.End; i++ {
				stmt := input.Stmts[i].AST
				if stmt.StatementType() != tree.TypeDDL {
					continue
				}
				if !seenFirst {
					seenFirst = true
					continue
				}
				pos := input.Positions[i]
				findings = append(findings, Finding{
					Message:  fmt.Sprintf("additional DDL (%s) in the same explicit transaction; CockroachDB runs each DDL as an online schema change and may leave the schema partially applied if any one fails", stmt.StatementTag()),
					Position: &pos,
					FixHint:  "Run each DDL in its own transaction (one BEGIN..COMMIT per DDL)",
				})
			}
		}
		return findings
	},
}

// ddlAndDMLInTxnRule flags any explicit BEGIN..COMMIT block that
// contains both a DDL statement and a DML statement, classified by
// the parser's tree.StatementType() (TypeDDL vs TypeDML). The schema
// change commits asynchronously, so the DML in the same txn may
// observe an inconsistent intermediate schema or the txn may abort
// with an error that is hard to reason about. The rule emits one
// finding per offending block, anchored at the BEGIN, regardless of
// how many DDL or DML statements the block contains.
var ddlAndDMLInTxnRule = MultiRule{
	ReasonCode: "DDL_AND_DML_IN_TXN",
	Severity:   SeverityMedium,
	Category:   "transactional_safety",
	Check: func(input MultiCheckInput) []Finding {
		var findings []Finding
		for _, blk := range explicitTxnBlocks(input.Stmts) {
			var hasDDL, hasDML bool
			for i := blk.Start + 1; i < blk.End; i++ {
				switch input.Stmts[i].AST.StatementType() {
				case tree.TypeDDL:
					hasDDL = true
				case tree.TypeDML:
					hasDML = true
				}
			}
			if !hasDDL || !hasDML {
				continue
			}
			pos := input.Positions[blk.Start]
			findings = append(findings, Finding{
				Message:  "explicit transaction mixes DDL and DML; the asynchronous schema change can leave the DML observing an inconsistent intermediate schema",
				Position: &pos,
				FixHint:  "Split the transaction so DDL and DML run separately (one BEGIN..COMMIT per DDL, with DML in its own transactions)",
			})
		}
		return findings
	},
}
