// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package safety_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/safety"
)

func TestCheckReadOnlyExplain(t *testing.T) {
	tests := []struct {
		name           string
		sql            string
		expectedReject bool
		expectedTag    string
	}{
		// Read-only allowlist accepts.
		{name: "select", sql: "SELECT * FROM t"},
		{name: "select with where", sql: "SELECT id FROM t WHERE id = 1"},
		{name: "show", sql: "SHOW TABLES"},
		{name: "show create", sql: "SHOW CREATE TABLE t"},
		{name: "values", sql: "VALUES (1), (2)"},
		{name: "with cte", sql: "WITH cte AS (SELECT 1) SELECT * FROM cte"},

		// Read-only allowlist rejects DML writes.
		{name: "insert", sql: "INSERT INTO t VALUES (1)", expectedReject: true, expectedTag: "INSERT"},
		{name: "update", sql: "UPDATE t SET x = 1 WHERE id = 1", expectedReject: true, expectedTag: "UPDATE"},
		{name: "delete", sql: "DELETE FROM t WHERE id = 1", expectedReject: true, expectedTag: "DELETE"},
		{name: "truncate", sql: "TRUNCATE TABLE t", expectedReject: true, expectedTag: "TRUNCATE"},

		// Read-only allowlist rejects DDL.
		{name: "drop table", sql: "DROP TABLE users", expectedReject: true, expectedTag: "DROP TABLE"},
		{name: "create table", sql: "CREATE TABLE x (id INT PRIMARY KEY)", expectedReject: true, expectedTag: "CREATE TABLE"},
		{name: "alter table", sql: "ALTER TABLE x ADD COLUMN y INT", expectedReject: true, expectedTag: "ALTER TABLE"},

		// Read-only allowlist rejects DCL writes.
		{name: "grant", sql: "GRANT SELECT ON t TO bob", expectedReject: true, expectedTag: "GRANT"},
		{name: "revoke", sql: "REVOKE SELECT ON t FROM bob", expectedReject: true, expectedTag: "REVOKE"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := safety.Check(safety.ModeReadOnly, safety.OpExplain, tc.sql)
			require.NoError(t, err)

			if !tc.expectedReject {
				require.Nil(t, v, "expected statement to be admitted")
				return
			}
			require.NotNil(t, v, "expected statement to be rejected")
			require.Equal(t, tc.expectedTag, v.Tag)
			require.Equal(t, safety.ModeReadOnly, v.Mode)
			require.Equal(t, safety.OpExplain, v.Op)
			require.NotEmpty(t, v.Reason)
		})
	}
}

func TestCheckReadOnlyExplainDDL(t *testing.T) {
	// In read_only mode, OpExplainDDL rejects everything: DDL because
	// it modifies schema, non-DDL because explain-ddl requires a DDL
	// inner stmt. The two reject reasons differ — that distinction is
	// the load-bearing assertion below.
	tests := []struct {
		name           string
		sql            string
		expectedReason string
	}{
		{
			name:           "ddl rejected with escalation hint",
			sql:            "ALTER TABLE x ADD COLUMN y INT",
			expectedReason: "rerun with --mode=safe_write or --mode=full_access",
		},
		{
			name:           "create table rejected with escalation hint",
			sql:            "CREATE TABLE x (id INT PRIMARY KEY)",
			expectedReason: "rerun with --mode=safe_write or --mode=full_access",
		},
		{
			name:           "select rejected as non-ddl",
			sql:            "SELECT 1",
			expectedReason: "explain_ddl requires a DDL statement",
		},
		{
			name:           "insert rejected as non-ddl",
			sql:            "INSERT INTO t VALUES (1)",
			expectedReason: "explain_ddl requires a DDL statement",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := safety.Check(safety.ModeReadOnly, safety.OpExplainDDL, tc.sql)
			require.NoError(t, err)
			require.NotNil(t, v)
			require.Contains(t, v.Reason, tc.expectedReason)
		})
	}
}

func TestCheckRejectsUnimplementedModes(t *testing.T) {
	// safe_write and full_access parse successfully (per ParseMode)
	// but Check rejects them until issues #28/#29. The rejection
	// reason names the mode so an agent can recognise the
	// "not implemented" condition vs a real classification miss.
	tests := []struct {
		name string
		mode safety.Mode
	}{
		{name: "safe_write not yet implemented", mode: safety.ModeSafeWrite},
		{name: "full_access not yet implemented", mode: safety.ModeFullAccess},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := safety.Check(tc.mode, safety.OpExplain, "SELECT 1")
			require.NoError(t, err)
			require.NotNil(t, v, "non-read_only modes are not admitted yet")
			require.Contains(t, v.Reason, "not yet implemented")
			require.Equal(t, tc.mode, v.Mode)
		})
	}
}

func TestCheckMultiStatementShortCircuits(t *testing.T) {
	// First statement is fine, second is a write — Check must reject
	// at the first violation and report the offending tag, not the
	// last admitted one.
	v, err := safety.Check(safety.ModeReadOnly, safety.OpExplain,
		"SELECT 1; DELETE FROM t WHERE id = 1; SELECT 2")
	require.NoError(t, err)
	require.NotNil(t, v)
	require.Equal(t, "DELETE", v.Tag)
}

func TestCheckParseErrorPropagates(t *testing.T) {
	// A malformed input must surface as a parse error, not a safety
	// violation: the user gets a real syntax diagnostic and not a
	// misleading "rejected by allowlist" message.
	v, err := safety.Check(safety.ModeReadOnly, safety.OpExplain, "SELEKT broken")
	require.Error(t, err)
	require.Nil(t, v)
}

func TestCheckRejectsEmptyInput(t *testing.T) {
	// parser.Parse("") returns zero stmts and no error, which without
	// an explicit empty-batch guard would be (nil, nil) — i.e.
	// "permitted". Pin the defensive rejection so a regression that
	// removes the guard is loud.
	tests := []struct {
		name string
		sql  string
	}{
		{name: "empty string", sql: ""},
		{name: "whitespace only", sql: "   \n\t  "},
		{name: "comment only", sql: "-- nothing here"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := safety.Check(safety.ModeReadOnly, safety.OpExplain, tc.sql)
			require.NoError(t, err)
			require.NotNil(t, v, "empty input must not bypass the gate")
			require.Contains(t, v.Reason, "no statements parsed")
		})
	}
}

func TestCheckRejectsNestedExplain(t *testing.T) {
	// tree.CanWriteData/CanModifySchema do not descend into
	// *Explain/*ExplainAnalyze AST nodes, so a caller wrapping their
	// write in EXPLAIN ANALYZE would otherwise sneak through the AST
	// allowlist (the cluster's BEGIN READ ONLY catches it at runtime,
	// but defense-in-depth says reject before any cluster contact).
	tests := []struct {
		name string
		sql  string
	}{
		{name: "explain analyze write", sql: "EXPLAIN ANALYZE INSERT INTO t VALUES (1)"},
		{name: "explain analyze ddl", sql: "EXPLAIN ANALYZE ALTER TABLE t ADD COLUMN x INT"},
		{name: "explain select", sql: "EXPLAIN SELECT 1"},
		{name: "explain analyze select", sql: "EXPLAIN ANALYZE SELECT 1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := safety.Check(safety.ModeReadOnly, safety.OpExplain, tc.sql)
			require.NoError(t, err)
			require.NotNil(t, v, "nested EXPLAIN must be rejected at the AST layer")
			require.Contains(t, v.Reason, "nested EXPLAIN")
		})
	}
}
