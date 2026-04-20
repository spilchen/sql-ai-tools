// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sqlparse

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name               string
		input              string
		expectedLen        int
		expectedType       StatementType
		expectedTag        string
		expectedNormalized string
		expectedErr        string
	}{
		{
			name:               "SELECT is DML",
			input:              "SELECT 1",
			expectedLen:        1,
			expectedType:       StatementTypeDML,
			expectedTag:        "SELECT",
			expectedNormalized: "SELECT _",
		},
		{
			name:               "INSERT is DML",
			input:              "INSERT INTO t VALUES (1)",
			expectedLen:        1,
			expectedType:       StatementTypeDML,
			expectedTag:        "INSERT",
			expectedNormalized: "INSERT INTO t VALUES (_)",
		},
		{
			name:               "UPDATE is DML",
			input:              "UPDATE t SET a = 1",
			expectedLen:        1,
			expectedType:       StatementTypeDML,
			expectedTag:        "UPDATE",
			expectedNormalized: "UPDATE t SET a = _",
		},
		{
			name:               "DELETE is DML",
			input:              "DELETE FROM t",
			expectedLen:        1,
			expectedType:       StatementTypeDML,
			expectedTag:        "DELETE",
			expectedNormalized: "DELETE FROM t",
		},
		{
			name:               "CREATE TABLE is DDL",
			input:              "CREATE TABLE t (a INT)",
			expectedLen:        1,
			expectedType:       StatementTypeDDL,
			expectedTag:        "CREATE TABLE",
			expectedNormalized: "CREATE TABLE t (a INT8)",
		},
		{
			name:         "ALTER TABLE is DDL",
			input:        "ALTER TABLE t ADD COLUMN b INT",
			expectedLen:  1,
			expectedType: StatementTypeDDL,
			expectedTag:  "ALTER TABLE",
		},
		{
			name:         "DROP TABLE is DDL",
			input:        "DROP TABLE t",
			expectedLen:  1,
			expectedType: StatementTypeDDL,
			expectedTag:  "DROP TABLE",
		},
		{
			name:         "GRANT is DCL",
			input:        "GRANT SELECT ON TABLE t TO u",
			expectedLen:  1,
			expectedType: StatementTypeDCL,
			expectedTag:  "GRANT",
		},
		{
			name:               "BEGIN is TCL",
			input:              "BEGIN",
			expectedLen:        1,
			expectedType:       StatementTypeTCL,
			expectedTag:        "BEGIN",
			expectedNormalized: "BEGIN TRANSACTION",
		},
		{
			name:               "COMMIT is TCL",
			input:              "COMMIT",
			expectedLen:        1,
			expectedType:       StatementTypeTCL,
			expectedTag:        "COMMIT",
			expectedNormalized: "COMMIT TRANSACTION",
		},
		{
			name:        "multi-statement input",
			input:       "SELECT 1; CREATE TABLE t (a INT)",
			expectedLen: 2,
		},
		{
			name:        "empty input returns empty slice",
			input:       "",
			expectedLen: 0,
		},
		{
			name:        "parse error",
			input:       "SELECTT 1",
			expectedErr: "syntax error",
		},
		{
			name:               "normalized hides string literal",
			input:              "SELECT * FROM t WHERE name = 'alice'",
			expectedLen:        1,
			expectedType:       StatementTypeDML,
			expectedTag:        "SELECT",
			expectedNormalized: "SELECT * FROM t WHERE name = '_'",
		},
		{
			name:               "normalized hides multiple constants",
			input:              "SELECT * FROM t WHERE id = 1 AND status = 'active'",
			expectedLen:        1,
			expectedType:       StatementTypeDML,
			expectedTag:        "SELECT",
			expectedNormalized: "SELECT * FROM t WHERE (id = _) AND (status = '_')",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stmts, err := Classify(tc.input)
			if tc.expectedErr != "" {
				require.ErrorContains(t, err, tc.expectedErr)
				return
			}
			require.NoError(t, err)
			require.Len(t, stmts, tc.expectedLen)

			if tc.expectedLen > 0 && tc.expectedType != "" {
				require.Equal(t, tc.expectedType, stmts[0].StatementType)
				require.Equal(t, tc.expectedTag, stmts[0].Tag)
				require.NotEmpty(t, stmts[0].SQL)
			}
			if tc.expectedNormalized != "" {
				require.Equal(t, tc.expectedNormalized, stmts[0].Normalized)
			}
		})
	}
}

func TestClassifyMultiStatementDetails(t *testing.T) {
	stmts, err := Classify("SELECT 1; CREATE TABLE t (a INT)")
	require.NoError(t, err)
	require.Len(t, stmts, 2)

	require.Equal(t, StatementTypeDML, stmts[0].StatementType)
	require.Equal(t, "SELECT", stmts[0].Tag)

	require.Equal(t, StatementTypeDDL, stmts[1].StatementType)
	require.Equal(t, "CREATE TABLE", stmts[1].Tag)
}
