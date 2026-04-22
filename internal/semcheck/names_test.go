// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package semcheck

import (
	"testing"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/catalog"
	"github.com/spilchen/sql-ai-tools/internal/diag"
)

// usersOnlyCatalog returns a catalog containing a single "users" table.
// It is the default fixture for cases that only need to distinguish
// "known" from "unknown" tables.
func usersOnlyCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.Load([]catalog.SchemaSource{{
		SQL:   "CREATE TABLE users (id INT PRIMARY KEY)",
		Label: "test",
	}})
	require.NoError(t, err)
	return cat
}

func TestCheckTableNames(t *testing.T) {
	tests := []struct {
		name             string
		sql              string
		expectedCount    int
		expectedMissing  string // first missing-table name (when expectedCount > 0)
		expectedAvail    []string
		expectedCategory string
	}{
		{
			name:          "known table",
			sql:           "SELECT * FROM users",
			expectedCount: 0,
		},
		{
			name:             "unknown table in SELECT",
			sql:              "SELECT * FROM usrs",
			expectedCount:    1,
			expectedMissing:  "usrs",
			expectedAvail:    []string{"users"},
			expectedCategory: diag.CategoryUnknownTable,
		},
		{
			name:            "unknown table in INSERT",
			sql:             "INSERT INTO usrs (id) VALUES (1)",
			expectedCount:   1,
			expectedMissing: "usrs",
		},
		{
			name:            "unknown table in UPDATE",
			sql:             "UPDATE usrs SET id = 2 WHERE id = 1",
			expectedCount:   1,
			expectedMissing: "usrs",
		},
		{
			name:            "unknown table in DELETE",
			sql:             "DELETE FROM usrs WHERE id = 1",
			expectedCount:   1,
			expectedMissing: "usrs",
		},
		{
			name:          "join known and unknown reports only unknown",
			sql:           "SELECT * FROM users u JOIN orders o ON u.id = o.user_id",
			expectedCount: 1,
		},
		{
			name:          "CTE shadows missing table",
			sql:           "WITH t AS (SELECT 1) SELECT * FROM t",
			expectedCount: 0,
		},
		{
			name:          "subquery in FROM flags inner unknown",
			sql:           "SELECT * FROM (SELECT * FROM usrs) x",
			expectedCount: 1,
		},
		{
			name:          "numeric TableRef skipped",
			sql:           "SELECT * FROM [1 AS t]",
			expectedCount: 0,
		},
		{
			name:          "no table references",
			sql:           "SELECT 1",
			expectedCount: 0,
		},
		{
			name:          "qualified name resolves on bare object",
			sql:           "SELECT * FROM public.users",
			expectedCount: 0,
		},
		{
			name:          "case-insensitive lookup",
			sql:           "SELECT * FROM USERS",
			expectedCount: 0,
		},
		{
			name:          "duplicate references collapse",
			sql:           "SELECT * FROM usrs WHERE EXISTS (SELECT 1 FROM usrs)",
			expectedCount: 1,
		},
		{
			name:            "join with both sides unknown",
			sql:             "SELECT * FROM unk1 u JOIN unk2 v ON u.id = v.id",
			expectedCount:   2,
			expectedMissing: "unk1",
		},
		{
			name:          "two distinct unknowns in one statement",
			sql:           "SELECT * FROM missingA, missingB",
			expectedCount: 2,
		},
		{
			name:          "UNION flags both sides",
			sql:           "SELECT * FROM unk1 UNION SELECT * FROM unk2",
			expectedCount: 2,
		},
		{
			name:          "CTE body still flags inner unknown",
			sql:           "WITH x AS (SELECT * FROM usrs) SELECT * FROM x",
			expectedCount: 1,
		},
		{
			name:          "DELETE USING flags unknown",
			sql:           "DELETE FROM users USING unk WHERE users.id = unk.id",
			expectedCount: 1,
		},
		{
			name:          "INSERT ON CONFLICT does not double-flag target",
			sql:           "INSERT INTO users (id) VALUES (1) ON CONFLICT (id) DO NOTHING",
			expectedCount: 0,
		},
	}

	cat := usersOnlyCatalog(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stmts, err := parser.Parse(tc.sql)
			require.NoError(t, err, "parse must succeed")

			errs := CheckTableNames(stmts, tc.sql, cat)
			require.Len(t, errs, tc.expectedCount)

			if tc.expectedCount == 0 {
				return
			}
			first := errs[0]
			require.Equal(t, unknownTableCode, first.Code)
			if tc.expectedMissing != "" {
				require.Contains(t, first.Message, tc.expectedMissing)
			}
			if tc.expectedCategory != "" {
				require.Equal(t, tc.expectedCategory, first.Category)
			}
			if tc.expectedAvail != nil {
				avail, ok := first.Context["available_tables"].([]string)
				require.True(t, ok, "available_tables must be []string")
				require.Equal(t, tc.expectedAvail, avail)
			}
		})
	}
}

// TestCheckTableNamesPosition verifies that the reported position
// points at the first occurrence of the missing table name in the SQL.
func TestCheckTableNamesPosition(t *testing.T) {
	cat := usersOnlyCatalog(t)
	sql := "SELECT * FROM usrs"
	stmts, err := parser.Parse(sql)
	require.NoError(t, err)

	errs := CheckTableNames(stmts, sql, cat)
	require.Len(t, errs, 1)
	require.NotNil(t, errs[0].Position)
	require.Equal(t, 1, errs[0].Position.Line)
	require.Equal(t, 15, errs[0].Position.Column)
	require.Equal(t, 14, errs[0].Position.ByteOffset)
}

// TestCheckTableNamesAliasScopePerStatement verifies that an alias
// introduced in one statement does not leak into the next, and that
// the reported position points at the second statement's "u" rather
// than the substring inside "users" in the first.
func TestCheckTableNamesAliasScopePerStatement(t *testing.T) {
	cat := usersOnlyCatalog(t)
	sql := "SELECT * FROM users u; SELECT * FROM u"
	stmts, err := parser.Parse(sql)
	require.NoError(t, err)

	errs := CheckTableNames(stmts, sql, cat)
	require.Len(t, errs, 1)
	require.Contains(t, errs[0].Message, `"u"`)
	require.NotNil(t, errs[0].Position)
	// The standalone "u" sits at byte 37 in the input, not byte 14
	// (which would be the "u" inside "users" in the first statement).
	require.Equal(t, 37, errs[0].Position.ByteOffset)
}

// TestCheckTableNamesNilCatalog verifies that a nil catalog is treated
// as a no-op rather than panicking. CheckTableNames is meant to be
// guarded by the caller; this is defense in depth.
func TestCheckTableNamesNilCatalog(t *testing.T) {
	stmts, err := parser.Parse("SELECT * FROM usrs")
	require.NoError(t, err)
	require.Empty(t, CheckTableNames(stmts, "SELECT * FROM usrs", nil))
}

// TestCheckTableNamesEmptyStatements verifies the no-statements path.
func TestCheckTableNamesEmptyStatements(t *testing.T) {
	cat := usersOnlyCatalog(t)
	require.Empty(t, CheckTableNames(nil, "", cat))
}
