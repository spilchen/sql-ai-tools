// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package sqlparse wraps the cockroachdb-parser module to provide
// SQL parsing utilities consumed by both the CLI and MCP layers.
// The primary entry point is Classify, which parses a SQL string
// and returns a per-statement classification (DDL/DML/DCL/TCL),
// the statement tag (e.g. "SELECT", "ALTER TABLE"), the original
// SQL text, and a normalized form with literal constants replaced
// by placeholders.
package sqlparse

import (
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/tree"
)

// StatementType is the human-readable classification for a SQL
// statement. Values correspond to the tree.StatementType enum in
// cockroachdb-parser with the "Type" prefix stripped.
type StatementType string

// StatementType constants.
const (
	StatementTypeDDL     StatementType = "DDL"
	StatementTypeDML     StatementType = "DML"
	StatementTypeDCL     StatementType = "DCL"
	StatementTypeTCL     StatementType = "TCL"
	StatementTypeUnknown StatementType = "UNKNOWN"
)

// ClassifiedStatement is the per-statement result of parsing and
// classifying a SQL string. It is the JSON-serializable shape
// embedded in both the CLI envelope's Data field and the MCP tool
// result.
type ClassifiedStatement struct {
	StatementType StatementType `json:"statement_type"`
	Tag           string        `json:"tag"`
	SQL           string        `json:"sql"`
	// Normalized is the SQL text with literal constants replaced by
	// placeholders, produced by tree.FormatStatementHideConstants.
	// Numeric constants become _ and string literals become '_'
	// (e.g. "SELECT * FROM t WHERE id = _"). Structurally identical
	// queries that differ only in constant values share the same
	// normalized form.
	Normalized string `json:"normalized"`
}

// Classify parses sql using the CockroachDB parser and returns one
// ClassifiedStatement per parsed statement. Parse errors are returned
// as a Go error; the caller decides how to surface them (as
// output.Error entries in CLI mode, or as a tool-level error in MCP
// mode).
//
// An empty input returns an empty slice with no error.
func Classify(sql string) ([]ClassifiedStatement, error) {
	stmts, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}

	result := make([]ClassifiedStatement, len(stmts))
	for i, stmt := range stmts {
		result[i] = ClassifiedStatement{
			StatementType: mapStatementType(stmt.AST.StatementType()),
			Tag:           stmt.AST.StatementTag(),
			SQL:           stmt.SQL,
			Normalized:    tree.FormatStatementHideConstants(stmt.AST),
		}
	}
	return result, nil
}

// mapStatementType converts a tree.StatementType enum value to its
// human-readable string constant.
func mapStatementType(t tree.StatementType) StatementType {
	switch t {
	case tree.TypeDDL:
		return StatementTypeDDL
	case tree.TypeDML:
		return StatementTypeDML
	case tree.TypeDCL:
		return StatementTypeDCL
	case tree.TypeTCL:
		return StatementTypeTCL
	default:
		return StatementTypeUnknown
	}
}
