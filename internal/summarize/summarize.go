// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package summarize produces a structured, AST-only description of a
// SQL statement: the operation, the tables it touches, the WHERE
// predicates, the joins, the columns it mutates, and a delegated risk
// level from the risk package.
//
// It is the deterministic "what does this statement do?" companion to
// the risk package's "what is dangerous about it?". Like risk, it
// works purely from the cockroachdb-parser AST and never connects to a
// cluster.
//
// Example:
//
//	s, _ := summarize.Summarize("DELETE FROM orders WHERE status='x'")
//	// s[0].Operation == OpDelete
//	// s[0].Tables    == []string{"orders"}
//	// s[0].Predicates == []string{"status = 'x'"}
//	// s[0].RiskLevel == risk.SeverityInfo
package summarize

import (
	"strings"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser/statements"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/tree"

	"github.com/spilchen/sql-ai-tools/internal/risk"
)

// Operation classifies the top-level kind of a statement. The values
// are part of the wire format — adding new ones is fine, renaming
// existing ones is a breaking change.
type Operation string

// Operation values. OpOther is the catch-all for statement kinds that
// summarize does not structurally decompose; the StatementTag is
// surfaced via Summary.Tag so agents can still distinguish e.g.
// DROP TABLE from CREATE TABLE.
const (
	OpSelect Operation = "SELECT"
	OpInsert Operation = "INSERT"
	OpUpsert Operation = "UPSERT"
	OpUpdate Operation = "UPDATE"
	OpDelete Operation = "DELETE"
	OpOther  Operation = "OTHER"
)

// Summary is the per-statement result returned by Summarize. It is the
// JSON-serializable shape embedded in both the CLI envelope's Data
// field and any future MCP tool result.
//
// Field discipline:
//   - Tables, Predicates, Joins, AffectedColumns, ReferencedColumns
//     are emitted as empty JSON arrays rather than null when there
//     are no entries, so consumers can iterate without nil checks.
//   - AffectedColumns contains only columns mutated by DML: the
//     INSERT explicit column list, the UPDATE SET targets, and (for
//     INSERT ... ON CONFLICT DO UPDATE) the conflict-resolution
//     SET targets. It is empty for DELETE and SELECT.
//   - ReferencedColumns is the full read-and-write footprint: every
//     column the statement names in any expression position (SELECT
//     projection, WHERE, JOIN ON, GROUP BY, HAVING, ORDER BY,
//     RETURNING, ON CONFLICT body, plus subquery and CTE bodies)
//     unioned with AffectedColumns. It is therefore a superset of
//     AffectedColumns whenever any mutated columns are known.
//     Known gap: JOIN USING (col) names are stored as a NameList,
//     not an Expr, and are not surfaced.
//   - SelectStar is true when the statement's outermost projection
//     uses "*" or "t.*" (and for INSERT ... SELECT, when the embedded
//     SELECT does). When set, ReferencedColumns is a lower bound:
//     summarize never expands a star against a catalog because it has
//     no schema. Function-argument stars like count(*) do not set
//     this flag — they don't introduce an unenumerated footprint.
//   - RiskLevel is the highest severity reported by risk.Analyze for
//     the statement; risk.SeverityInfo is the baseline meaning "no
//     risks detected", not "an info-level risk was detected".
type Summary struct {
	Operation         Operation     `json:"operation"`
	Tag               string        `json:"tag"`
	Tables            []string      `json:"tables"`
	Predicates        []string      `json:"predicates"`
	Joins             []Join        `json:"joins"`
	AffectedColumns   []string      `json:"affected_columns"`
	ReferencedColumns []string      `json:"referenced_columns"`
	SelectStar        bool          `json:"select_star"`
	RiskLevel         risk.Severity `json:"risk_level"`
	Position          risk.Position `json:"position"`
}

// Join describes one JOIN clause inside a statement.
//
// Left and Right are best-effort table names: they hold the bare table
// name (or alias when present) when the side resolves to an
// AliasedTableExpr backed by a TableName, and are empty for nested
// joins or subquery sources.
//
// Condition holds the rendered ON expression, "USING (col1, col2)",
// "NATURAL", or empty for a CROSS join.
type Join struct {
	Type      string `json:"type"`
	Left      string `json:"left,omitempty"`
	Right     string `json:"right,omitempty"`
	Condition string `json:"condition,omitempty"`
}

// Summarize parses sql and returns one Summary per statement, in
// source order. Parse errors are returned to the caller; partial
// summaries are not produced.
func Summarize(sql string) ([]Summary, error) {
	stmts, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}
	return Parsed(stmts, sql), nil
}

// Parsed produces summaries for already-parsed statements. sql is the
// original source text — required for positionFromSQL to locate each
// statement's line/column. Exposed so callers that already invoked
// parser.Parse (e.g. to also run version.Inspect on the same AST) can
// reuse the parsed output rather than reparsing. Mirrors the
// Classify/ClassifyParsed split in package sqlparse; the shorter name
// avoids stuttering with the package name.
func Parsed(stmts statements.Statements, sql string) []Summary {
	summaries := make([]Summary, 0, len(stmts))
	offset := 0
	for _, stmt := range stmts {
		pos := positionFromSQL(sql, stmt.SQL, &offset)
		s := summarizeStatement(stmt.AST)
		s.Position = pos
		s.RiskLevel = riskLevelFor(stmt.SQL)
		summaries = append(summaries, s)
	}
	return summaries
}

// summarizeStatement summarizes one already-parsed statement. Position
// and RiskLevel are left at their zero values; the public Summarize
// entry point fills them in because both require the full SQL text.
func summarizeStatement(stmt tree.Statement) Summary {
	s := Summary{
		Tag:               stmt.StatementTag(),
		Tables:            collectTables(stmt),
		Predicates:        []string{},
		Joins:             []Join{},
		AffectedColumns:   []string{},
		ReferencedColumns: []string{},
	}

	switch n := stmt.(type) {
	case *tree.Select:
		s.Operation = OpSelect
		s.Predicates = wherePredicates(selectWhere(n))
		s.Joins = collectJoins(stmt)
	case *tree.Insert:
		if n.OnConflict.IsUpsertAlias() {
			s.Operation = OpUpsert
		} else {
			s.Operation = OpInsert
		}
		s.AffectedColumns = insertAffectedColumns(n)
		// INSERT ... SELECT can have a WHERE inside the embedded
		// SELECT; surface those predicates so agents see the full
		// row-selection footprint.
		if n.Rows != nil {
			s.Predicates = wherePredicates(selectWhere(n.Rows))
			s.Joins = collectJoins(n.Rows)
		}
	case *tree.Update:
		s.Operation = OpUpdate
		s.Predicates = wherePredicates(n.Where)
		s.AffectedColumns = updateTargets(n.Exprs)
		s.Joins = collectJoins(stmt)
	case *tree.Delete:
		s.Operation = OpDelete
		s.Predicates = wherePredicates(n.Where)
		s.Joins = collectJoins(stmt)
	default:
		s.Operation = OpOther
	}

	// Read footprint is computed independently of the per-op switch
	// above so the same walker handles SELECT/DML/CTEs uniformly.
	// Mutated columns are then unioned in to preserve the documented
	// "referenced ⊇ affected" invariant for INSERT/UPDATE.
	s.ReferencedColumns = mergeColumns(collectReferences(stmt), s.AffectedColumns)
	s.SelectStar = hasProjectionStar(stmt)
	return s
}

// mergeColumns returns refs ∪ extras, preserving refs' order and
// appending any extras not already present (case-insensitive). Both
// slices are treated as immutable; the result is freshly allocated so
// callers may store either input independently.
func mergeColumns(refs, extras []string) []string {
	if len(refs) == 0 && len(extras) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(refs)+len(extras))
	seen := make(map[string]struct{}, len(refs)+len(extras))
	for _, r := range refs {
		key := strings.ToLower(r)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, r)
	}
	for _, e := range extras {
		key := strings.ToLower(e)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, e)
	}
	return out
}

// selectWhere returns the WHERE clause attached to the leaf
// SelectClause inside a Select, or nil for set operations
// (UNION/INTERSECT/EXCEPT) and VALUES clauses, which have no top-level
// WHERE.
func selectWhere(sel *tree.Select) *tree.Where {
	if sel == nil {
		return nil
	}
	sc, ok := sel.Select.(*tree.SelectClause)
	if !ok {
		return nil
	}
	return sc.Where
}

// wherePredicates renders w as one string per top-level conjunct,
// splitting on AndExpr. Returns the empty slice when w is nil so
// JSON callers see [] rather than null. Conjuncts are rendered with
// FmtSimple to match how a human would write them
// (e.g. "status = 'x'", not "status = 'x':::STRING").
func wherePredicates(w *tree.Where) []string {
	if w == nil || w.Expr == nil {
		return []string{}
	}
	var out []string
	walkConjuncts(w.Expr, &out)
	if out == nil {
		return []string{}
	}
	return out
}

func walkConjuncts(expr tree.Expr, out *[]string) {
	if and, ok := expr.(*tree.AndExpr); ok {
		walkConjuncts(and.Left, out)
		walkConjuncts(and.Right, out)
		return
	}
	*out = append(*out, tree.AsStringWithFlags(expr, tree.FmtSimple))
}

// updateTargets returns the LHS column names of an UPDATE's SET list,
// in source order. Tuple assignments ("(a, b) = (1, 2)") flatten into
// individual names.
func updateTargets(exprs tree.UpdateExprs) []string {
	var out []string
	for _, e := range exprs {
		if e == nil {
			continue
		}
		out = append(out, nameListToStrings(e.Names)...)
	}
	if out == nil {
		return []string{}
	}
	return out
}

// insertAffectedColumns returns the columns an INSERT statement
// mutates: the explicit (col, col, …) list plus, for ON CONFLICT DO
// UPDATE, the SET LHS columns of the conflict-resolution UPDATE.
// ON CONFLICT DO NOTHING contributes nothing. Insert columns come
// first (the row being inserted), then any DO UPDATE SET targets
// not already listed; case-insensitive dedup keeps the per-column
// shape stable when an INSERT and its ON CONFLICT body name the
// same column.
func insertAffectedColumns(n *tree.Insert) []string {
	out := nameListToStrings(n.Columns)
	if n.OnConflict == nil || n.OnConflict.DoNothing {
		return out
	}
	return mergeColumns(out, updateTargets(n.OnConflict.Exprs))
}

// nameListToStrings is NameList.ToStrings normalized to never return
// nil — JSON consumers expect []string{} for "no names".
func nameListToStrings(names tree.NameList) []string {
	if len(names) == 0 {
		return []string{}
	}
	return names.ToStrings()
}

// riskLevelFor delegates to risk.Analyze and reduces the findings to
// the highest severity seen. risk.SeverityInfo is the "no risks
// detected" baseline; we never invent findings here.
//
// risk.Analyze re-parses stmtSQL internally. We pass the per-statement
// SQL substring produced by our own earlier parse, so a re-parse error
// is not expected in practice; if one occurs we collapse to
// SeverityInfo rather than fail the whole summary, since the structured
// fields above are still valid and a missing risk_level is less bad
// than dropping the entire statement. Note that this path is silent —
// a future change should consider surfacing risk-analysis failures as
// envelope warnings.
func riskLevelFor(stmtSQL string) risk.Severity {
	findings, err := risk.Analyze(stmtSQL)
	if err != nil {
		return risk.SeverityInfo
	}
	highest := risk.SeverityInfo
	for _, f := range findings {
		if severityRank(f.Severity) > severityRank(highest) {
			highest = f.Severity
		}
	}
	return highest
}

// severityRank orders severities from least to most urgent so we can
// reduce a findings slice to its peak. The constants are strings whose
// lexical order doesn't match severity, so we map them explicitly.
// Renaming a Severity value here without updating registry.go is a
// silent bug — keep this switch in sync with risk.Severity*.
func severityRank(s risk.Severity) int {
	switch s {
	case risk.SeverityCritical:
		return 5
	case risk.SeverityHigh:
		return 4
	case risk.SeverityMedium:
		return 3
	case risk.SeverityLow:
		return 2
	case risk.SeverityInfo:
		return 1
	}
	return 0
}

// positionFromSQL is duplicated from the unexported helper of the same
// name in the risk package. Promoting it to a shared helper is not yet
// justified by a third caller; consolidate when one appears.
//
// On a fallback (stmtSQL not located in fullSQL — e.g. a parser
// round-trip mismatch where AST.SQL is no longer byte-identical to the
// source) we still advance *offset past the missing statement so
// subsequent statements don't all collapse to the same byte offset.
// Position.Line == 0 signals the fallback; consumers can treat it as
// "unknown" rather than the misleading 1:1.
func positionFromSQL(fullSQL, stmtSQL string, offset *int) risk.Position {
	idx := strings.Index(fullSQL[*offset:], stmtSQL)
	if idx < 0 {
		pos := risk.Position{Line: 0, Column: 0, ByteOffset: *offset}
		*offset += len(stmtSQL)
		return pos
	}
	byteOff := *offset + idx
	*offset = byteOff + len(stmtSQL)

	prefix := fullSQL[:byteOff]
	line := strings.Count(prefix, "\n") + 1
	lastNL := strings.LastIndex(prefix, "\n") // works when lastNL == -1 (no newlines)
	col := byteOff - lastNL
	return risk.Position{Line: line, Column: col, ByteOffset: byteOff}
}
