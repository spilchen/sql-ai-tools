// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package risk provides a rule-based engine for detecting dangerous SQL
// patterns by inspecting the AST produced by the CockroachDB parser.
//
// The engine is organized around three concepts:
//
//   - Rule: a named check that inspects a single parsed statement and
//     returns zero or more findings. Each rule has a severity, reason
//     code, and category.
//   - Registry: an ordered collection of rules. Its Analyze method
//     parses a SQL string and runs every rule against every statement.
//   - Finding: a single risk detected in a SQL statement, carrying a
//     machine-readable reason code, human-readable message, position
//     within the input, and a fix hint.
//
// Callers that want the built-in rule set can use the package-level
// Analyze function, which delegates to DefaultRules().
package risk

import (
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser/statements"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/tree"
)

// Severity classifies the urgency of a finding. Values are ordered from
// most to least urgent and are part of the wire format — renaming or
// reordering them is a breaking change.
type Severity string

// Severity constants, ordered from most to least urgent.
const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
	SeverityInfo     Severity = "info"
)

// Finding is a single risk detected by a rule. The struct is the
// JSON-serializable shape embedded in both the CLI envelope's Data
// field and the MCP tool result.
type Finding struct {
	ReasonCode string    `json:"reason_code"`
	Severity   Severity  `json:"severity"`
	Message    string    `json:"message"`
	Position   *Position `json:"position,omitempty"`
	FixHint    string    `json:"fix_hint"`
}

// Position identifies a location within the original SQL input.
// Line and Column are 1-based; ByteOffset is 0-based.
type Position struct {
	Line       int `json:"line"`
	Column     int `json:"column"`
	ByteOffset int `json:"byte_offset"`
}

// CheckInput is the context passed to a rule's check function for a
// single parsed statement.
type CheckInput struct {
	AST      tree.Statement
	StmtSQL  string
	Position Position
}

// Rule defines an AST-only risk detection rule. The Check function
// inspects a single parsed statement and returns any findings. Rules
// are expected to return nil when the statement is not relevant
// (e.g. a DELETE rule receiving a SELECT AST).
type Rule struct {
	ReasonCode string
	Severity   Severity
	Category   string
	Check      func(input CheckInput) []Finding
}

// MultiCheckInput is the context passed to a multi-statement rule's
// check function. Stmts is the full parsed statement list for the SQL
// passed to Analyze; Positions[i] is the location of Stmts[i] within
// the original SQL. The two slices have the same length and are
// indexed in parallel.
type MultiCheckInput struct {
	Stmts     statements.Statements
	Positions []Position
}

// MultiRule defines a risk detection rule that needs to look across
// more than one parsed statement at a time (e.g. spotting multiple
// DDL statements inside a single explicit BEGIN..COMMIT block). The
// Check function is invoked once per Analyze call with the full
// statement list and returns any findings; it is expected to return
// nil when the input does not contain anything relevant.
type MultiRule struct {
	ReasonCode string
	Severity   Severity
	Category   string
	Check      func(input MultiCheckInput) []Finding
}

// Registry holds an ordered set of per-statement rules and an ordered
// set of multi-statement rules, and runs them against parsed SQL.
// Built by NewRegistry and immutable afterward; rule evaluation order
// matches the order provided to the constructor (per-statement rules
// run first, in registration order, for each statement; multi-rules
// run afterward, in registration order, once per Analyze call).
type Registry struct {
	rules      []Rule
	multiRules []MultiRule
}

// NewRegistry creates a registry from the given per-statement rules
// and multi-statement rules. Either slice may be nil. Rules are
// evaluated in the order provided: per-statement rules in
// registration order for each statement, then multi-statement rules
// in registration order once per Analyze call. It panics if any rule
// has a nil Check function, an empty ReasonCode, or a ReasonCode that
// duplicates any other registered rule (across both slices).
func NewRegistry(rules []Rule, multiRules []MultiRule) *Registry {
	seen := make(map[string]bool, len(rules)+len(multiRules))
	for _, rule := range rules {
		if rule.Check == nil {
			panic(fmt.Sprintf("risk: rule %q has nil Check function", rule.ReasonCode))
		}
		if rule.ReasonCode == "" {
			panic("risk: rule has empty ReasonCode")
		}
		if seen[rule.ReasonCode] {
			panic(fmt.Sprintf("risk: duplicate ReasonCode %q", rule.ReasonCode))
		}
		seen[rule.ReasonCode] = true
	}
	for _, rule := range multiRules {
		if rule.Check == nil {
			panic(fmt.Sprintf("risk: multi-rule %q has nil Check function", rule.ReasonCode))
		}
		if rule.ReasonCode == "" {
			panic("risk: multi-rule has empty ReasonCode")
		}
		if seen[rule.ReasonCode] {
			panic(fmt.Sprintf("risk: duplicate ReasonCode %q", rule.ReasonCode))
		}
		seen[rule.ReasonCode] = true
	}
	return &Registry{rules: rules, multiRules: multiRules}
}

// Analyze parses sql and applies every registered rule. Per-statement
// rules run first, in statement order, with rules evaluated in
// registration order within each statement; multi-statement rules run
// afterward, in registration order, each invoked once over the full
// statement list. Findings are returned in production order, so all
// per-statement findings precede all multi-statement findings. The
// registry stamps each finding's ReasonCode and Severity from the
// rule that produced it, so individual rules only need to set
// Message, Position, and FixHint.
func (r *Registry) Analyze(sql string) ([]Finding, error) {
	stmts, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}
	return r.AnalyzeParsed(stmts, sql), nil
}

// AnalyzeParsed runs every registered rule against already-parsed
// statements. sql is the original source text — required for
// positionFromSQL to locate each statement's line/column. Exposed so
// callers that already invoked parser.Parse (e.g. to also run
// version.Inspect on the same AST) can reuse the parsed output rather
// than reparsing. Returns no error: parsing happened upstream.
func (r *Registry) AnalyzeParsed(stmts statements.Statements, sql string) []Finding {
	findings := []Finding{}
	positions := make([]Position, len(stmts))
	offset := 0
	for i, stmt := range stmts {
		pos := positionFromSQL(sql, stmt.SQL, &offset)
		positions[i] = pos
		input := CheckInput{
			AST:      stmt.AST,
			StmtSQL:  stmt.SQL,
			Position: pos,
		}
		for _, rule := range r.rules {
			for _, f := range rule.Check(input) {
				f.ReasonCode = rule.ReasonCode
				f.Severity = rule.Severity
				findings = append(findings, f)
			}
		}
	}

	multiInput := MultiCheckInput{Stmts: stmts, Positions: positions}
	for _, rule := range r.multiRules {
		for _, f := range rule.Check(multiInput) {
			f.ReasonCode = rule.ReasonCode
			f.Severity = rule.Severity
			findings = append(findings, f)
		}
	}
	return findings
}

// Analyze is a convenience function that runs the default rule set
// against sql. It is equivalent to
// NewRegistry(DefaultRules(), DefaultMultiRules()).Analyze(sql).
func Analyze(sql string) ([]Finding, error) {
	return NewRegistry(DefaultRules(), DefaultMultiRules()).Analyze(sql)
}

// AnalyzeParsed is a convenience function that runs the default rule
// set against already-parsed statements. It is equivalent to
// NewRegistry(DefaultRules(), DefaultMultiRules()).AnalyzeParsed(stmts, sql).
func AnalyzeParsed(stmts statements.Statements, sql string) []Finding {
	return NewRegistry(DefaultRules(), DefaultMultiRules()).AnalyzeParsed(stmts, sql)
}

// positionFromSQL computes the Position of stmtSQL within fullSQL,
// searching forward from *offset. It advances *offset past the
// matched statement so successive calls find successive statements.
func positionFromSQL(fullSQL, stmtSQL string, offset *int) Position {
	idx := strings.Index(fullSQL[*offset:], stmtSQL)
	if idx < 0 {
		return Position{Line: 1, Column: 1, ByteOffset: *offset}
	}
	byteOff := *offset + idx
	*offset = byteOff + len(stmtSQL)

	prefix := fullSQL[:byteOff]
	line := strings.Count(prefix, "\n") + 1
	lastNL := strings.LastIndex(prefix, "\n")
	col := byteOff - lastNL // works when lastNL == -1 (no newlines)
	return Position{Line: line, Column: col, ByteOffset: byteOff}
}
