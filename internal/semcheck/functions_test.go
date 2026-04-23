// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package semcheck

import (
	"strings"
	"testing"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/output"
)

func TestCheckFunctionNames(t *testing.T) {
	tests := []struct {
		name              string
		sql               string
		expectedCount     int
		expectedFirstName string
		expectedFirstCode string
	}{
		{
			name:          "known builtin call passes",
			sql:           "SELECT now()",
			expectedCount: 0,
		},
		{
			name:          "case insensitive match",
			sql:           "SELECT NoW()",
			expectedCount: 0,
		},
		{
			name:              "unknown bare name flagged",
			sql:               "SELECT now_()",
			expectedCount:     1,
			expectedFirstName: "now_",
			expectedFirstCode: "42883",
		},
		{
			name:          "schema-qualified call skipped",
			sql:           "SELECT pg_catalog.fake_function_xyz()",
			expectedCount: 0,
		},
		{
			name:              "unknown function in WHERE clause",
			sql:               "SELECT 1 FROM t WHERE bogus_fn(x) = 1",
			expectedCount:     1,
			expectedFirstName: "bogus_fn",
		},
		{
			name:              "unknown function in INSERT VALUES",
			sql:               "INSERT INTO t (a) VALUES (bogus_fn())",
			expectedCount:     1,
			expectedFirstName: "bogus_fn",
		},
		{
			name:              "unknown function in UPDATE SET",
			sql:               "UPDATE t SET a = bogus_fn() WHERE id = 1",
			expectedCount:     1,
			expectedFirstName: "bogus_fn",
		},
		{
			name:              "unknown function in DELETE WHERE",
			sql:               "DELETE FROM t WHERE bogus_fn(x) = 1",
			expectedCount:     1,
			expectedFirstName: "bogus_fn",
		},
		{
			name:              "unknown function in JOIN ON",
			sql:               "SELECT 1 FROM a JOIN b ON bogus_fn(a.x) = b.y",
			expectedCount:     1,
			expectedFirstName: "bogus_fn",
		},
		{
			name:              "unknown function in subquery",
			sql:               "SELECT (SELECT bogus_fn() FROM t)",
			expectedCount:     1,
			expectedFirstName: "bogus_fn",
		},
		{
			name:              "unknown function in RETURNING",
			sql:               "INSERT INTO t (a) VALUES (1) RETURNING bogus_fn(a)",
			expectedCount:     1,
			expectedFirstName: "bogus_fn",
		},
		{
			name:          "deduplicates across statements",
			sql:           "SELECT now_(); SELECT now_()",
			expectedCount: 1,
		},
		{
			name:          "deduplicates across calls in one statement",
			sql:           "SELECT bogus_fn(), bogus_fn()",
			expectedCount: 1,
		},
		{
			name:          "two distinct typos produce two errors",
			sql:           "SELECT now_(), uppr('hi')",
			expectedCount: 2,
		},
		{
			name:          "nested calls inside known function",
			sql:           "SELECT length(bogus_fn())",
			expectedCount: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stmts, err := parser.Parse(tc.sql)
			require.NoError(t, err)

			errs := CheckFunctionNames(stmts, tc.sql)
			require.Len(t, errs, tc.expectedCount)
			if tc.expectedCount == 0 {
				return
			}
			if tc.expectedFirstCode != "" {
				require.Equal(t, tc.expectedFirstCode, errs[0].Code)
			}
			if tc.expectedFirstName != "" {
				require.Contains(t, errs[0].Message, tc.expectedFirstName)
			}
		})
	}
}

func TestCheckFunctionNamesEmptyStatements(t *testing.T) {
	require.Empty(t, CheckFunctionNames(nil, ""))
}

func TestCheckFunctionNamesDiagnosticShape(t *testing.T) {
	const sql = "SELECT now_()"
	stmts, err := parser.Parse(sql)
	require.NoError(t, err)

	errs := CheckFunctionNames(stmts, sql)
	require.Len(t, errs, 1)
	e := errs[0]

	require.Equal(t, "42883", e.Code)
	require.Equal(t, output.SeverityError, e.Severity)
	require.Equal(t, diag.CategoryUnknownFunction, e.Category)
	require.Contains(t, e.Message, "now_")

	require.NotNil(t, e.Position, "position should be populated for word-boundary match")
	require.Equal(t, 1, e.Position.Line)
	// "SELECT " is 7 bytes, then "now_" starts at byte 7, column 8.
	require.Equal(t, 8, e.Position.Column)

	avail, ok := e.Context["available_functions"].([]string)
	require.True(t, ok, "available_functions should be a []string")
	require.NotEmpty(t, avail)
	require.LessOrEqual(t, len(avail), availableFunctionsSampleSize)

	require.NotEmpty(t, e.Suggestions, "did-you-mean suggestions should be present for a near-match")
	require.Equal(t, "now", e.Suggestions[0].Replacement)
	require.Greater(t, e.Suggestions[0].Confidence, 0.5)
}

func TestCheckFunctionNamesPositionWordBoundary(t *testing.T) {
	// "now_" appears inside the column alias and inside a string
	// literal; positionFor must locate the actual call site, which is
	// the third occurrence (preceded by a SELECT and a non-identifier
	// boundary).
	const sql = "SELECT 'now_value' AS now_alias, now_()"
	stmts, err := parser.Parse(sql)
	require.NoError(t, err)

	errs := CheckFunctionNames(stmts, sql)
	require.Len(t, errs, 1)
	require.NotNil(t, errs[0].Position)
	// The call site is the only occurrence flanked on both sides by
	// non-identifier bytes (', ' before, '(' after); the alias
	// "now_alias" extends past "now_" so its boundary check fails.
	idx := strings.Index(sql, "now_(")
	require.Equal(t, idx, errs[0].Position.ByteOffset)
}

// TestCheckFunctionNamesDedupReportsFirstPosition pins the doc-comment
// claim that "the first reference's source position is reported" when
// the same unknown name appears in multiple statements. Without this
// test, a future refactor could silently start reporting the last (or
// an arbitrary) occurrence.
func TestCheckFunctionNamesDedupReportsFirstPosition(t *testing.T) {
	const sql = "SELECT 1; SELECT bogus_fn(); SELECT bogus_fn()"
	stmts, err := parser.Parse(sql)
	require.NoError(t, err)

	errs := CheckFunctionNames(stmts, sql)
	require.Len(t, errs, 1)
	require.NotNil(t, errs[0].Position)
	firstCall := strings.Index(sql, "bogus_fn(")
	require.Equal(t, firstCall, errs[0].Position.ByteOffset)
}

func TestCheckFunctionNamesAvailableFunctionsRanked(t *testing.T) {
	// Sampling should rank by Levenshtein distance, so the closest
	// names ("now") appear before unrelated names ("avg", "min", ...).
	const sql = "SELECT now_()"
	stmts, err := parser.Parse(sql)
	require.NoError(t, err)

	errs := CheckFunctionNames(stmts, sql)
	require.Len(t, errs, 1)
	avail, ok := errs[0].Context["available_functions"].([]string)
	require.True(t, ok)
	require.Contains(t, avail, "now", "the closest known builtin should be in the sample")
	require.Equal(t, "now", avail[0], "closest match should rank first")
}
