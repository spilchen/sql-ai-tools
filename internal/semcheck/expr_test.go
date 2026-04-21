// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package semcheck

import (
	"testing"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/stretchr/testify/require"
)

func TestCheckExprTypes(t *testing.T) {
	tests := []struct {
		name          string
		sql           string
		expectedCount int
		expectedMsg   string
	}{
		{
			name:          "valid literal arithmetic",
			sql:           "SELECT 1 + 2",
			expectedCount: 0,
		},
		{
			name:          "type mismatch binary op",
			sql:           "SELECT 1 + 'hello'",
			expectedCount: 1,
			expectedMsg:   "unsupported binary operator",
		},
		{
			name:          "valid string concatenation",
			sql:           "SELECT 'a' || 'b'",
			expectedCount: 0,
		},
		{
			name:          "column reference skipped",
			sql:           "SELECT a + 1 FROM t",
			expectedCount: 0,
		},
		{
			name:          "function call skipped",
			sql:           "SELECT length('hello')",
			expectedCount: 0,
		},
		{
			name:          "subquery skipped",
			sql:           "SELECT (SELECT 1) + 'hello'",
			expectedCount: 0,
		},
		{
			name:          "GREATEST does not panic",
			sql:           "SELECT GREATEST(1, 2)",
			expectedCount: 0,
		},
		{
			name:          "valid CAST",
			sql:           "SELECT CAST(1 AS STRING)",
			expectedCount: 0,
		},
		{
			name:          "valid COALESCE",
			sql:           "SELECT COALESCE(1, 2)",
			expectedCount: 0,
		},
		{
			name:          "star expression skipped",
			sql:           "SELECT * FROM t",
			expectedCount: 0,
		},
		{
			name:          "multiple statements mixed",
			sql:           "SELECT 1 + 2; SELECT 1 + 'x'",
			expectedCount: 1,
		},
		{
			name:          "multiple type errors in one statement",
			sql:           "SELECT 1 + 'a', 2 + 'b'",
			expectedCount: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stmts, err := parser.Parse(tc.sql)
			require.NoError(t, err, "parse must succeed")

			errs := CheckExprTypes(stmts, tc.sql)
			require.Len(t, errs, tc.expectedCount)

			if tc.expectedMsg != "" && tc.expectedCount > 0 {
				require.Contains(t, errs[0].Message, tc.expectedMsg)
			}
		})
	}
}

func TestCheckExprTypesPosition(t *testing.T) {
	sql := "SELECT 1 + 'hello'"
	stmts, err := parser.Parse(sql)
	require.NoError(t, err)

	errs := CheckExprTypes(stmts, sql)
	require.Len(t, errs, 1)
	require.NotNil(t, errs[0].Position)
	require.Equal(t, 1, errs[0].Position.Line)
	require.NotEmpty(t, errs[0].Code)
}

func TestCheckExprTypesEmptyStatements(t *testing.T) {
	errs := CheckExprTypes(nil, "")
	require.Empty(t, errs)
}
