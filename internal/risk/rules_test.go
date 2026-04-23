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
		{
			name:                "two DDLs in explicit txn flag",
			sql:                 "BEGIN; CREATE TABLE a (id INT PRIMARY KEY); CREATE TABLE b (id INT PRIMARY KEY); COMMIT;",
			expectedReasonCodes: []string{"MULTIPLE_DDL_IN_TXN"},
		},
		{
			name:                "three DDLs in explicit txn flag twice",
			sql:                 "BEGIN; CREATE TABLE a (id INT PRIMARY KEY); CREATE TABLE b (id INT PRIMARY KEY); CREATE TABLE c (id INT PRIMARY KEY); COMMIT;",
			expectedReasonCodes: []string{"MULTIPLE_DDL_IN_TXN", "MULTIPLE_DDL_IN_TXN"},
		},
		{
			name:                "single DDL in explicit txn is safe",
			sql:                 "BEGIN; CREATE TABLE a (id INT PRIMARY KEY); COMMIT;",
			expectedReasonCodes: nil,
		},
		{
			name:                "two DDLs in implicit txn do not flag",
			sql:                 "CREATE TABLE a (id INT PRIMARY KEY); CREATE TABLE b (id INT PRIMARY KEY);",
			expectedReasonCodes: nil,
		},
		{
			name:                "DDL then DML in explicit txn flags",
			sql:                 "BEGIN; CREATE TABLE a (id INT PRIMARY KEY); SELECT 1; COMMIT;",
			expectedReasonCodes: []string{"DDL_AND_DML_IN_TXN"},
		},
		{
			name:                "DML then DDL in explicit txn flags",
			sql:                 "BEGIN; INSERT INTO t VALUES (1); ALTER TABLE t ADD COLUMN x INT; COMMIT;",
			expectedReasonCodes: []string{"DDL_AND_DML_IN_TXN"},
		},
		{
			name:                "explicit txn with only DDLs does not trigger DDL_AND_DML",
			sql:                 "BEGIN; CREATE TABLE a (id INT PRIMARY KEY); CREATE TABLE b (id INT PRIMARY KEY); COMMIT;",
			expectedReasonCodes: []string{"MULTIPLE_DDL_IN_TXN"},
		},
		{
			name:                "explicit txn with only DML does not flag",
			sql:                 "BEGIN; INSERT INTO t VALUES (1); SELECT id FROM t WHERE id = 1; COMMIT;",
			expectedReasonCodes: nil,
		},
		{
			name:                "ROLLBACK closes the txn block for cross-stmt rules",
			sql:                 "BEGIN; CREATE TABLE a (id INT PRIMARY KEY); SELECT 1; ROLLBACK;",
			expectedReasonCodes: []string{"DDL_AND_DML_IN_TXN"},
		},
		{
			name:                "multi-DDL plus DML emits both rules",
			sql:                 "BEGIN; CREATE TABLE a (id INT PRIMARY KEY); CREATE TABLE b (id INT PRIMARY KEY); INSERT INTO a VALUES (1); COMMIT;",
			expectedReasonCodes: []string{"MULTIPLE_DDL_IN_TXN", "DDL_AND_DML_IN_TXN"},
		},
		{
			name:                "SAVEPOINT cockroach_restart flags",
			sql:                 "SAVEPOINT cockroach_restart",
			expectedReasonCodes: []string{"SAVEPOINT_COCKROACH_RESTART"},
		},
		{
			name:                "RELEASE SAVEPOINT cockroach_restart flags",
			sql:                 "RELEASE SAVEPOINT cockroach_restart",
			expectedReasonCodes: []string{"SAVEPOINT_COCKROACH_RESTART"},
		},
		{
			name:                "ROLLBACK TO SAVEPOINT cockroach_restart flags",
			sql:                 "ROLLBACK TO SAVEPOINT cockroach_restart",
			expectedReasonCodes: []string{"SAVEPOINT_COCKROACH_RESTART"},
		},
		{
			name:                "SAVEPOINT cockroach_restart case-insensitive",
			sql:                 `SAVEPOINT "Cockroach_Restart"`,
			expectedReasonCodes: []string{"SAVEPOINT_COCKROACH_RESTART"},
		},
		{
			name:                "user-named savepoint is safe",
			sql:                 "SAVEPOINT my_app_sp",
			expectedReasonCodes: nil,
		},
		{
			name:                "explicit txn with multi-DDL plus multi-DML emits one DDL_AND_DML finding",
			sql:                 "BEGIN; CREATE TABLE a (id INT PRIMARY KEY); INSERT INTO t VALUES (1); SELECT id FROM t WHERE id = 1; COMMIT;",
			expectedReasonCodes: []string{"DDL_AND_DML_IN_TXN"},
		},
		{
			name:                "DDL plus UPDATE flags DDL_AND_DML",
			sql:                 "BEGIN; CREATE TABLE u (id INT PRIMARY KEY); UPDATE u SET id = 2 WHERE id = 1; COMMIT;",
			expectedReasonCodes: []string{"DDL_AND_DML_IN_TXN"},
		},
		{
			name:                "DDL plus DELETE flags DDL_AND_DML",
			sql:                 "BEGIN; CREATE TABLE d (id INT PRIMARY KEY); DELETE FROM d WHERE id = 1; COMMIT;",
			expectedReasonCodes: []string{"DDL_AND_DML_IN_TXN"},
		},
		{
			name:                "DDL plus UPSERT flags DDL_AND_DML",
			sql:                 "BEGIN; CREATE TABLE u (id INT PRIMARY KEY); UPSERT INTO u VALUES (1); COMMIT;",
			expectedReasonCodes: []string{"DDL_AND_DML_IN_TXN"},
		},
		{
			name:                "two consecutive explicit txns each with multi-DDL flag separately",
			sql:                 "BEGIN; CREATE TABLE a (id INT PRIMARY KEY); CREATE TABLE b (id INT PRIMARY KEY); COMMIT; BEGIN; CREATE TABLE c (id INT PRIMARY KEY); CREATE TABLE d (id INT PRIMARY KEY); COMMIT;",
			expectedReasonCodes: []string{"MULTIPLE_DDL_IN_TXN", "MULTIPLE_DDL_IN_TXN"},
		},
		{
			name:                "BEGIN without matching COMMIT still scans the unterminated block",
			sql:                 "BEGIN; CREATE TABLE a (id INT PRIMARY KEY); CREATE TABLE b (id INT PRIMARY KEY);",
			expectedReasonCodes: []string{"MULTIPLE_DDL_IN_TXN"},
		},
		{
			name:                "surplus COMMIT before any BEGIN does not panic and emits nothing for the multi-rules",
			sql:                 "COMMIT; CREATE TABLE x (id INT PRIMARY KEY);",
			expectedReasonCodes: nil,
		},
		{
			name:                "surplus ROLLBACK before any BEGIN does not panic and emits nothing for the multi-rules",
			sql:                 "ROLLBACK; CREATE TABLE x (id INT PRIMARY KEY);",
			expectedReasonCodes: nil,
		},
		{
			name:                "nested BEGIN: outer block sees both DDLs and flags MULTIPLE_DDL once",
			sql:                 "BEGIN; CREATE TABLE a (id INT PRIMARY KEY); BEGIN; CREATE TABLE b (id INT PRIMARY KEY); COMMIT; COMMIT;",
			expectedReasonCodes: []string{"MULTIPLE_DDL_IN_TXN"},
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
		{
			name:             "MULTIPLE_DDL_IN_TXN is high",
			sql:              "BEGIN; CREATE TABLE a (id INT PRIMARY KEY); CREATE TABLE b (id INT PRIMARY KEY); COMMIT;",
			expectedSeverity: SeverityHigh,
		},
		{
			name:             "DDL_AND_DML_IN_TXN is medium",
			sql:              "BEGIN; CREATE TABLE a (id INT PRIMARY KEY); SELECT 1; COMMIT;",
			expectedSeverity: SeverityMedium,
		},
		{
			name:             "SAVEPOINT_COCKROACH_RESTART is medium",
			sql:              "SAVEPOINT cockroach_restart",
			expectedSeverity: SeverityMedium,
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

// TestMultipleDDLInTxnFindingPositions pins down the rule's "one
// finding per *extra* DDL beyond the first, anchored at that DDL"
// contract. Each statement is on its own line so position.Line
// uniquely identifies which statement the finding refers to.
func TestMultipleDDLInTxnFindingPositions(t *testing.T) {
	sql := "BEGIN;\n" +
		"CREATE TABLE a (id INT PRIMARY KEY);\n" +
		"CREATE TABLE b (id INT PRIMARY KEY);\n" +
		"CREATE TABLE c (id INT PRIMARY KEY);\n" +
		"COMMIT;"
	findings, err := Analyze(sql)
	require.NoError(t, err)
	require.Len(t, findings, 2)
	require.Equal(t, "MULTIPLE_DDL_IN_TXN", findings[0].ReasonCode)
	require.Equal(t, "MULTIPLE_DDL_IN_TXN", findings[1].ReasonCode)
	require.NotNil(t, findings[0].Position)
	require.NotNil(t, findings[1].Position)
	// First finding points at the second DDL (line 3); second points
	// at the third DDL (line 4). The first DDL on line 2 is never
	// itself flagged.
	require.Equal(t, 3, findings[0].Position.Line)
	require.Equal(t, 4, findings[1].Position.Line)
}

// TestDDLAndDMLInTxnFindingAnchoredAtBegin pins down the
// "one finding per offending block, anchored at the BEGIN" contract.
func TestDDLAndDMLInTxnFindingAnchoredAtBegin(t *testing.T) {
	sql := "SELECT 1;\n" +
		"BEGIN;\n" +
		"CREATE TABLE a (id INT PRIMARY KEY);\n" +
		"INSERT INTO a VALUES (1);\n" +
		"COMMIT;"
	findings, err := Analyze(sql)
	require.NoError(t, err)
	require.Len(t, findings, 1)
	require.Equal(t, "DDL_AND_DML_IN_TXN", findings[0].ReasonCode)
	require.NotNil(t, findings[0].Position)
	// BEGIN is on line 2; finding must point there, not at the inner
	// DDL (line 3) or DML (line 4).
	require.Equal(t, 2, findings[0].Position.Line)
}
