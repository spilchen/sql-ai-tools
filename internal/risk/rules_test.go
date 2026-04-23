// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package risk

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAnalyze(t *testing.T) {
	tests := []struct {
		name                string
		sql                 string
		expectedReasonCodes []string
	}{
		{
			name:                "DELETE without WHERE",
			sql:                 "DELETE FROM users",
			expectedReasonCodes: []string{"DELETE_NO_WHERE"},
		},
		{
			name:                "DELETE with WHERE is safe",
			sql:                 "DELETE FROM users WHERE id = 1",
			expectedReasonCodes: nil,
		},
		{
			name:                "DELETE with LIMIT is safe",
			sql:                 "DELETE FROM users LIMIT 100",
			expectedReasonCodes: nil,
		},
		{
			name:                "UPDATE without WHERE",
			sql:                 "UPDATE users SET name = 'x'",
			expectedReasonCodes: []string{"UPDATE_NO_WHERE"},
		},
		{
			name:                "UPDATE with WHERE is safe",
			sql:                 "UPDATE users SET name = 'x' WHERE id = 1",
			expectedReasonCodes: nil,
		},
		{
			name:                "UPDATE with LIMIT is safe",
			sql:                 "UPDATE users SET name = 'x' LIMIT 100",
			expectedReasonCodes: nil,
		},
		{
			name:                "DROP TABLE",
			sql:                 "DROP TABLE users",
			expectedReasonCodes: []string{"DROP_TABLE"},
		},
		{
			name:                "DROP TABLE IF EXISTS multiple tables",
			sql:                 "DROP TABLE IF EXISTS a, b",
			expectedReasonCodes: []string{"DROP_TABLE"},
		},
		{
			name:                "SELECT star",
			sql:                 "SELECT * FROM users",
			expectedReasonCodes: []string{"SELECT_STAR"},
		},
		{
			name:                "SELECT specific columns is safe",
			sql:                 "SELECT id, name FROM users",
			expectedReasonCodes: nil,
		},
		{
			name:                "SELECT qualified star",
			sql:                 "SELECT t.* FROM t",
			expectedReasonCodes: []string{"SELECT_STAR"},
		},
		{
			name:                "INSERT is safe",
			sql:                 "INSERT INTO t VALUES (1)",
			expectedReasonCodes: nil,
		},
		{
			name:                "CREATE TABLE is safe",
			sql:                 "CREATE TABLE t (id INT PRIMARY KEY)",
			expectedReasonCodes: nil,
		},
		{
			name:                "multi-statement with multiple risks",
			sql:                 "DELETE FROM users; SELECT * FROM t; UPDATE t SET x = 1",
			expectedReasonCodes: []string{"DELETE_NO_WHERE", "SELECT_STAR", "UPDATE_NO_WHERE"},
		},
		{
			name:                "DROP DATABASE",
			sql:                 "DROP DATABASE d",
			expectedReasonCodes: []string{"DROP_DATABASE"},
		},
		{
			name:                "DROP DATABASE IF EXISTS CASCADE",
			sql:                 "DROP DATABASE IF EXISTS d CASCADE",
			expectedReasonCodes: []string{"DROP_DATABASE"},
		},
		{
			name:                "ALTER TABLE DROP COLUMN",
			sql:                 "ALTER TABLE t DROP COLUMN c",
			expectedReasonCodes: []string{"ALTER_TABLE_DROP_COLUMN"},
		},
		{
			name:                "ALTER TABLE DROP COLUMN multiple columns yields per-column findings",
			sql:                 "ALTER TABLE t DROP COLUMN a, DROP COLUMN b",
			expectedReasonCodes: []string{"ALTER_TABLE_DROP_COLUMN", "ALTER_TABLE_DROP_COLUMN"},
		},
		{
			name:                "ALTER TABLE mixed commands flags only DROP COLUMN",
			sql:                 "ALTER TABLE t ADD COLUMN x INT, DROP COLUMN y",
			expectedReasonCodes: []string{"ALTER_TABLE_DROP_COLUMN"},
		},
		{
			name:                "ALTER TABLE ADD COLUMN is safe",
			sql:                 "ALTER TABLE t ADD COLUMN c INT",
			expectedReasonCodes: nil,
		},
		{
			name:                "SELECT FOR UPDATE without WHERE or LIMIT",
			sql:                 "SELECT id FROM t FOR UPDATE",
			expectedReasonCodes: []string{"SELECT_FOR_UPDATE_NO_WHERE"},
		},
		{
			name:                "SELECT FOR UPDATE with star fires both rules",
			sql:                 "SELECT * FROM t FOR UPDATE",
			expectedReasonCodes: []string{"SELECT_FOR_UPDATE_NO_WHERE", "SELECT_STAR"},
		},
		{
			name:                "SELECT FOR UPDATE with WHERE is safe",
			sql:                 "SELECT id FROM t WHERE id = 1 FOR UPDATE",
			expectedReasonCodes: nil,
		},
		{
			name:                "SELECT FOR UPDATE with LIMIT is safe",
			sql:                 "SELECT id FROM t LIMIT 10 FOR UPDATE",
			expectedReasonCodes: nil,
		},
		{
			name:                "SELECT FOR SHARE without WHERE or LIMIT",
			sql:                 "SELECT id FROM t FOR SHARE",
			expectedReasonCodes: []string{"SELECT_FOR_SHARE_NO_WHERE"},
		},
		{
			name:                "SELECT FOR SHARE with WHERE is safe",
			sql:                 "SELECT id FROM t WHERE id = 1 FOR SHARE",
			expectedReasonCodes: nil,
		},
		{
			name:                "SELECT FOR SHARE with LIMIT is safe",
			sql:                 "SELECT id FROM t LIMIT 10 FOR SHARE",
			expectedReasonCodes: nil,
		},
		{
			name:                "SELECT FOR UPDATE on union conservatively flagged",
			sql:                 "(SELECT id FROM t WHERE id = 1) FOR UPDATE",
			expectedReasonCodes: []string{"SELECT_FOR_UPDATE_NO_WHERE"},
		},
		{
			name:                "multi-statement covers new rules in order",
			sql:                 "DROP DATABASE d; ALTER TABLE t DROP COLUMN c; SELECT id FROM t FOR UPDATE",
			expectedReasonCodes: []string{"DROP_DATABASE", "ALTER_TABLE_DROP_COLUMN", "SELECT_FOR_UPDATE_NO_WHERE"},
		},
		{
			name:                "TRUNCATE TABLE is critical",
			sql:                 "TRUNCATE TABLE users",
			expectedReasonCodes: []string{"TRUNCATE_TABLE"},
		},
		{
			name:                "TRUNCATE TABLE CASCADE is critical",
			sql:                 "TRUNCATE TABLE users CASCADE",
			expectedReasonCodes: []string{"TRUNCATE_TABLE"},
		},
		{
			name:                "TRUNCATE multiple tables yields one finding",
			sql:                 "TRUNCATE TABLE a, b",
			expectedReasonCodes: []string{"TRUNCATE_TABLE"},
		},
		{
			name:                "SERIAL primary key inline",
			sql:                 "CREATE TABLE t (id SERIAL PRIMARY KEY)",
			expectedReasonCodes: []string{"SERIAL_PRIMARY_KEY"},
		},
		{
			name:                "BIGSERIAL primary key inline",
			sql:                 "CREATE TABLE t (id BIGSERIAL PRIMARY KEY)",
			expectedReasonCodes: []string{"SERIAL_PRIMARY_KEY"},
		},
		{
			name:                "SMALLSERIAL primary key inline",
			sql:                 "CREATE TABLE t (id SMALLSERIAL PRIMARY KEY)",
			expectedReasonCodes: []string{"SERIAL_PRIMARY_KEY"},
		},
		{
			name:                "SERIAL non-PK column is safe",
			sql:                 "CREATE TABLE t (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), seq SERIAL)",
			expectedReasonCodes: nil,
		},
		{
			name:                "SERIAL inside table-level PRIMARY KEY constraint",
			sql:                 "CREATE TABLE t (a SERIAL, b INT, PRIMARY KEY (a, b))",
			expectedReasonCodes: []string{"SERIAL_PRIMARY_KEY"},
		},
		{
			name:                "UUID primary key is safe",
			sql:                 "CREATE TABLE t (id UUID PRIMARY KEY DEFAULT gen_random_uuid())",
			expectedReasonCodes: nil,
		},
		{
			name:                "missing primary key on plain CREATE TABLE",
			sql:                 "CREATE TABLE t (a INT, b STRING)",
			expectedReasonCodes: []string{"MISSING_PRIMARY_KEY"},
		},
		{
			name:                "table-level PRIMARY KEY constraint satisfies missing-PK rule",
			sql:                 "CREATE TABLE t (a INT, b INT, PRIMARY KEY (a, b))",
			expectedReasonCodes: nil,
		},
		{
			name:                "UNIQUE without PRIMARY KEY still missing PK",
			sql:                 "CREATE TABLE t (id INT, UNIQUE(id))",
			expectedReasonCodes: []string{"MISSING_PRIMARY_KEY"},
		},
		{
			name:                "CREATE TABLE AS exempt from missing-PK rule",
			sql:                 "CREATE TABLE copy AS SELECT 1 AS a",
			expectedReasonCodes: nil,
		},
		{
			name:                "OFFSET below threshold is safe",
			sql:                 "SELECT id FROM t ORDER BY id LIMIT 20 OFFSET 100",
			expectedReasonCodes: nil,
		},
		{
			name:                "OFFSET at threshold flags",
			sql:                 "SELECT id FROM t ORDER BY id LIMIT 20 OFFSET 1000",
			expectedReasonCodes: []string{"LARGE_OFFSET"},
		},
		{
			name:                "OFFSET well above threshold flags",
			sql:                 "SELECT id FROM t ORDER BY id LIMIT 20 OFFSET 50000",
			expectedReasonCodes: []string{"LARGE_OFFSET"},
		},
		{
			name:                "OFFSET as subquery does not flag",
			sql:                 "SELECT id FROM t LIMIT 20 OFFSET (SELECT 5000)",
			expectedReasonCodes: nil,
		},
		{
			name:                "OFFSET as scientific-notation literal flags",
			sql:                 "SELECT id FROM t LIMIT 20 OFFSET 1e4",
			expectedReasonCodes: []string{"LARGE_OFFSET"},
		},
		{
			name:                "OFFSET as decimal literal flags",
			sql:                 "SELECT id FROM t LIMIT 20 OFFSET 2000.0",
			expectedReasonCodes: []string{"LARGE_OFFSET"},
		},
		{
			name:                "OFFSET that overflows int64 still flags",
			sql:                 "SELECT id FROM t LIMIT 20 OFFSET 9999999999999999999",
			expectedReasonCodes: []string{"LARGE_OFFSET"},
		},
		{
			name:                "OFFSET as parameter placeholder does not flag",
			sql:                 "SELECT id FROM t LIMIT 20 OFFSET $1",
			expectedReasonCodes: nil,
		},
		{
			name:                "PREPARE TRANSACTION flags",
			sql:                 "PREPARE TRANSACTION 'tx1'",
			expectedReasonCodes: []string{"XA_PREPARED_TXN"},
		},
		{
			name:                "COMMIT PREPARED flags",
			sql:                 "COMMIT PREPARED 'tx1'",
			expectedReasonCodes: []string{"XA_PREPARED_TXN"},
		},
		{
			name:                "ROLLBACK PREPARED flags",
			sql:                 "ROLLBACK PREPARED 'tx1'",
			expectedReasonCodes: []string{"XA_PREPARED_TXN"},
		},
		{
			name:                "PREPARE statement (non-XA) is safe",
			sql:                 "PREPARE q AS SELECT 1",
			expectedReasonCodes: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			findings, err := Analyze(tc.sql)
			require.NoError(t, err)

			var codes []string
			for _, f := range findings {
				codes = append(codes, f.ReasonCode)
			}
			require.Equal(t, tc.expectedReasonCodes, codes)
		})
	}
}

func TestAnalyzeParseError(t *testing.T) {
	_, err := Analyze("SELECTT 1")
	require.Error(t, err)
}

func TestAnalyzeEmptyInput(t *testing.T) {
	findings, err := Analyze("")
	require.NoError(t, err)
	require.Empty(t, findings)
}

func TestFindingSeverity(t *testing.T) {
	tests := []struct {
		name             string
		sql              string
		expectedSeverity Severity
	}{
		{
			name:             "DELETE_NO_WHERE is critical",
			sql:              "DELETE FROM t",
			expectedSeverity: SeverityCritical,
		},
		{
			name:             "UPDATE_NO_WHERE is critical",
			sql:              "UPDATE t SET x = 1",
			expectedSeverity: SeverityCritical,
		},
		{
			name:             "DROP_TABLE is critical",
			sql:              "DROP TABLE t",
			expectedSeverity: SeverityCritical,
		},
		{
			name:             "SELECT_STAR is low",
			sql:              "SELECT * FROM t",
			expectedSeverity: SeverityLow,
		},
		{
			name:             "DROP_DATABASE is critical",
			sql:              "DROP DATABASE d",
			expectedSeverity: SeverityCritical,
		},
		{
			name:             "ALTER_TABLE_DROP_COLUMN is high",
			sql:              "ALTER TABLE t DROP COLUMN c",
			expectedSeverity: SeverityHigh,
		},
		{
			name:             "SELECT_FOR_UPDATE_NO_WHERE is critical",
			sql:              "SELECT id FROM t FOR UPDATE",
			expectedSeverity: SeverityCritical,
		},
		{
			name:             "SELECT_FOR_SHARE_NO_WHERE is high",
			sql:              "SELECT id FROM t FOR SHARE",
			expectedSeverity: SeverityHigh,
		},
		{
			name:             "TRUNCATE_TABLE is critical",
			sql:              "TRUNCATE TABLE t",
			expectedSeverity: SeverityCritical,
		},
		{
			name:             "SERIAL_PRIMARY_KEY is high",
			sql:              "CREATE TABLE t (id SERIAL PRIMARY KEY)",
			expectedSeverity: SeverityHigh,
		},
		{
			name:             "MISSING_PRIMARY_KEY is medium",
			sql:              "CREATE TABLE t (a INT)",
			expectedSeverity: SeverityMedium,
		},
		{
			name:             "LARGE_OFFSET is medium",
			sql:              "SELECT id FROM t LIMIT 10 OFFSET 5000",
			expectedSeverity: SeverityMedium,
		},
		{
			name:             "XA_PREPARED_TXN is high",
			sql:              "PREPARE TRANSACTION 'tx1'",
			expectedSeverity: SeverityHigh,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			findings, err := Analyze(tc.sql)
			require.NoError(t, err)
			require.Len(t, findings, 1)
			require.Equal(t, tc.expectedSeverity, findings[0].Severity)
		})
	}
}

func TestFindingPosition(t *testing.T) {
	findings, err := Analyze("SELECT 1;\nDELETE FROM users")
	require.NoError(t, err)
	require.Len(t, findings, 1)
	require.Equal(t, "DELETE_NO_WHERE", findings[0].ReasonCode)
	require.NotNil(t, findings[0].Position)
	require.Equal(t, 2, findings[0].Position.Line)
	require.Equal(t, 1, findings[0].Position.Column)
}

func TestFindingFixHint(t *testing.T) {
	findings, err := Analyze("DELETE FROM users")
	require.NoError(t, err)
	require.Len(t, findings, 1)
	require.NotEmpty(t, findings[0].FixHint)
}

func TestDropTableMessage(t *testing.T) {
	findings, err := Analyze("DROP TABLE users")
	require.NoError(t, err)
	require.Len(t, findings, 1)
	require.Contains(t, findings[0].Message, "users")
}

func TestDropDatabaseMessage(t *testing.T) {
	findings, err := Analyze("DROP DATABASE inventory")
	require.NoError(t, err)
	require.Len(t, findings, 1)
	require.Contains(t, findings[0].Message, "inventory")
}

func TestAlterTableDropColumnMessage(t *testing.T) {
	findings, err := Analyze("ALTER TABLE users DROP COLUMN email")
	require.NoError(t, err)
	require.Len(t, findings, 1)
	require.Contains(t, findings[0].Message, "users")
	require.Contains(t, findings[0].Message, "email")
}

func TestTruncateCascadeMessageMentionsForeignKeys(t *testing.T) {
	findings, err := Analyze("TRUNCATE TABLE users CASCADE")
	require.NoError(t, err)
	require.Len(t, findings, 1)
	require.Contains(t, findings[0].Message, "CASCADE")
	require.Contains(t, findings[0].Message, "foreign keys")
}
