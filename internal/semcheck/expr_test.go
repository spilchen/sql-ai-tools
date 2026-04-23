// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package semcheck

import (
	"testing"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/builtinstubs"
)

func init() {
	builtinstubs.Init("")
}

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
			name:          "builtin function resolved",
			sql:           "SELECT length('hello')",
			expectedCount: 0,
		},
		{
			name:          "builtin function wrong arg type",
			sql:           "SELECT upper(123)",
			expectedCount: 1,
			expectedMsg:   "unknown signature",
		},
		{
			// Unknown bare function names are owned by
			// CheckFunctionNames, which emits the structured
			// 42883 with suggestions. CheckExprTypes deliberately
			// skips the FuncExpr (see containsUnknownFunc) so the
			// user sees exactly one diagnostic per typo.
			name:          "unknown function skipped by type check",
			sql:           "SELECT now_()",
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

// TestCheckExprTypesUnknownFuncCrossTalk pins the contract documented
// on containsUnknownFunc: skipping a FuncExpr whose bare name is
// unresolved must not silence type errors in sibling sub-expressions.
// Without this guarantee, a single typo would suppress every
// type-mismatch in the same statement and the user would see a new
// error after each fix-and-retry.
func TestCheckExprTypesUnknownFuncCrossTalk(t *testing.T) {
	tests := []struct {
		name          string
		sql           string
		expectedCount int
		expectedMsg   string
	}{
		{
			name:          "type error in sibling of unknown call",
			sql:           "SELECT 1 + 'x', bogus_fn()",
			expectedCount: 1,
			expectedMsg:   "unsupported binary operator",
		},
		{
			name:          "type error nested in arg list of known call wrapping unknown",
			sql:           "SELECT length(bogus_fn(), 1+'x')",
			expectedCount: 1,
			expectedMsg:   "unsupported binary operator",
		},
		{
			name:          "type error in same binary expression as unknown call",
			sql:           "SELECT 1 + 'x' + bogus_fn()",
			expectedCount: 1,
			expectedMsg:   "unsupported binary operator",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stmts, err := parser.Parse(tc.sql)
			require.NoError(t, err)
			errs := CheckExprTypes(stmts, tc.sql)
			require.Len(t, errs, tc.expectedCount)
			require.Contains(t, errs[0].Message, tc.expectedMsg)
		})
	}
}
