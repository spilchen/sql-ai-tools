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

// usersAndOrdersCatalog returns a catalog with two tables that both
// have an "id" column — the minimum needed to exercise ambiguity. It
// is the default fixture for column-resolution cases that need more
// than the single-table usersOnlyCatalog.
func usersAndOrdersCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.Load([]catalog.SchemaSource{{
		SQL: `CREATE TABLE users (id INT PRIMARY KEY, name TEXT, email TEXT);
CREATE TABLE orders (id INT PRIMARY KEY, user_id INT, total INT);`,
		Label: "test",
	}})
	require.NoError(t, err)
	return cat
}

func TestCheckColumnNames(t *testing.T) {
	tests := []struct {
		name             string
		sql              string
		expectedCount    int
		expectedCode     string
		expectedCategory string
		expectedMessage  string
		expectedAvail    []string
	}{
		{
			name:          "known unqualified column",
			sql:           "SELECT name FROM users",
			expectedCount: 0,
		},
		{
			name:             "unknown unqualified column",
			sql:              "SELECT nme FROM users",
			expectedCount:    1,
			expectedCode:     unknownColumnCode,
			expectedCategory: diag.CategoryUnknownColumn,
			expectedMessage:  `"nme"`,
			expectedAvail:    []string{"id", "name", "email"},
		},
		{
			name:          "qualified column via alias",
			sql:           "SELECT u.name FROM users u",
			expectedCount: 0,
		},
		{
			name:             "unknown column via alias",
			sql:              "SELECT u.nme FROM users u",
			expectedCount:    1,
			expectedCode:     unknownColumnCode,
			expectedCategory: diag.CategoryUnknownColumn,
			expectedMessage:  `"nme"`,
		},
		{
			name:             "qualified by missing table alias",
			sql:              "SELECT z.name FROM users",
			expectedCount:    1,
			expectedCode:     unknownColumnCode,
			expectedCategory: diag.CategoryUnknownColumn,
			expectedMessage:  `missing FROM-clause entry for table "z"`,
		},
		{
			name:             "ambiguous unqualified column",
			sql:              "SELECT id FROM users JOIN orders ON users.id = orders.user_id",
			expectedCount:    1,
			expectedCode:     ambiguousColumnCode,
			expectedCategory: diag.CategoryAmbiguousReference,
			expectedMessage:  `"id"`,
		},
		{
			name:          "qualified disambiguates id",
			sql:           "SELECT users.id FROM users JOIN orders ON users.id = orders.user_id",
			expectedCount: 0,
		},
		{
			name:          "case-insensitive column lookup",
			sql:           "SELECT NAME FROM users",
			expectedCount: 0,
		},
		{
			name:          "unknown table suppresses cascade",
			sql:           "SELECT nme FROM nosuch",
			expectedCount: 0,
		},
		{
			name:          "CTE source skips column refs",
			sql:           "WITH t AS (SELECT 1) SELECT t.x FROM t",
			expectedCount: 0,
		},
		{
			name:          "subquery in FROM skips column refs",
			sql:           "SELECT x.foo FROM (SELECT id FROM users) x",
			expectedCount: 0,
		},
		{
			name:          "subquery in FROM still flags inner refs",
			sql:           "SELECT 1 FROM (SELECT nme FROM users) x",
			expectedCount: 1,
		},
		{
			name:          "correlated subquery resolves outer alias",
			sql:           "SELECT * FROM users u WHERE EXISTS (SELECT 1 FROM orders o WHERE o.user_id = u.id)",
			expectedCount: 0,
		},
		{
			name:          "WHERE references unknown column",
			sql:           "SELECT 1 FROM users WHERE nme = 'x'",
			expectedCount: 1,
		},
		{
			name:          "ORDER BY on SELECT-list alias",
			sql:           "SELECT name AS n FROM users ORDER BY n",
			expectedCount: 0,
		},
		{
			name:          "GROUP BY known column",
			sql:           "SELECT name, count(*) FROM users GROUP BY name",
			expectedCount: 0,
		},
		{
			name:          "HAVING unknown column",
			sql:           "SELECT count(*) FROM users HAVING nme > 0",
			expectedCount: 1,
		},
		{
			name:          "UPDATE SET unknown column",
			sql:           "UPDATE users SET nme = 'x' WHERE id = 1",
			expectedCount: 1,
		},
		{
			name:          "UPDATE SET known + WHERE unknown",
			sql:           "UPDATE users SET name = 'x' WHERE nme = 'y'",
			expectedCount: 1,
		},
		{
			name:          "INSERT target column unknown",
			sql:           "INSERT INTO users (id, nme) VALUES (1, 'x')",
			expectedCount: 1,
		},
		{
			name:          "INSERT target column known",
			sql:           "INSERT INTO users (id, name) VALUES (1, 'x')",
			expectedCount: 0,
		},
		{
			name:          "DELETE WHERE unknown column",
			sql:           "DELETE FROM users WHERE nme = 'x'",
			expectedCount: 1,
		},
		{
			name:          "duplicate refs collapse across statements",
			sql:           "SELECT nme FROM users; SELECT nme FROM users",
			expectedCount: 1,
		},
		{
			name:          "two distinct unknowns flagged",
			sql:           "SELECT nme, eml FROM users",
			expectedCount: 2,
		},
		{
			name:          "JOIN ON unknown column",
			sql:           "SELECT 1 FROM users JOIN orders ON users.id = orders.usr",
			expectedCount: 1,
		},
		{
			name:          "DELETE USING extra source resolves",
			sql:           "DELETE FROM users USING orders WHERE users.id = orders.user_id",
			expectedCount: 0,
		},
		{
			name:          "no FROM no checks",
			sql:           "SELECT 1",
			expectedCount: 0,
		},
		{
			name:          "star skipped",
			sql:           "SELECT * FROM users",
			expectedCount: 0,
		},
		{
			name:          "qualified star skipped",
			sql:           "SELECT u.* FROM users u",
			expectedCount: 0,
		},
		{
			name:          "UNION right branch flags unknown column",
			sql:           "SELECT id FROM users UNION SELECT nme FROM users",
			expectedCount: 1,
		},
		{
			name:          "UNION left branch flags unknown column",
			sql:           "SELECT nme FROM users UNION SELECT id FROM users",
			expectedCount: 1,
		},
		{
			name:          "UNION ORDER BY does not false-positive",
			sql:           "SELECT id FROM users UNION SELECT id FROM orders ORDER BY id",
			expectedCount: 0,
		},
		{
			name:          "INTERSECT both branches flag distinct unknowns",
			sql:           "SELECT nme FROM users INTERSECT SELECT eml FROM users",
			expectedCount: 2,
		},
		{
			name:          "VALUES inside INSERT flags unknown column ref",
			sql:           "INSERT INTO users (id) VALUES (nme)",
			expectedCount: 1,
		},
		{
			name:          "JOIN USING with known column",
			sql:           "SELECT * FROM users JOIN orders USING (id)",
			expectedCount: 0,
		},
		{
			name:          "JOIN USING with unknown column",
			sql:           "SELECT * FROM users JOIN orders USING (nope)",
			expectedCount: 1,
		},
		{
			name:          "INSERT ON CONFLICT DO UPDATE flags unknown SET column",
			sql:           "INSERT INTO users (id) VALUES (1) ON CONFLICT (id) DO UPDATE SET nme = 'x'",
			expectedCount: 1,
		},
		{
			name:          "INSERT ON CONFLICT target column unknown",
			sql:           "INSERT INTO users (id) VALUES (1) ON CONFLICT (nope) DO NOTHING",
			expectedCount: 1,
		},
		{
			name:          "INSERT ON CONFLICT excluded refs are skipped",
			sql:           "INSERT INTO users (id) VALUES (1) ON CONFLICT (id) DO UPDATE SET name = excluded.name",
			expectedCount: 0,
		},
		{
			name:          "INSERT RETURNING flags unknown column",
			sql:           "INSERT INTO users (id) VALUES (1) RETURNING nme",
			expectedCount: 1,
		},
		{
			name:          "UPDATE RETURNING flags unknown column",
			sql:           "UPDATE users SET name = 'x' WHERE id = 1 RETURNING nme",
			expectedCount: 1,
		},
		{
			name:          "DELETE RETURNING flags unknown column",
			sql:           "DELETE FROM users WHERE id = 1 RETURNING nme",
			expectedCount: 1,
		},
		{
			name:          "UPDATE FROM extra source resolves",
			sql:           "UPDATE users SET name = 'x' FROM orders WHERE users.id = orders.user_id",
			expectedCount: 0,
		},
		{
			name:          "UPDATE FROM unknown column on extra source",
			sql:           "UPDATE users SET name = 'x' FROM orders WHERE users.id = orders.nope",
			expectedCount: 1,
		},
		{
			name:          "DELETE USING with unknown column",
			sql:           "DELETE FROM users USING orders WHERE orders.nope = 1",
			expectedCount: 1,
		},
		{
			name:          "GROUP BY on SELECT-list alias",
			sql:           "SELECT name AS only_alias FROM users GROUP BY only_alias",
			expectedCount: 0,
		},
		{
			name:          "qualified case-insensitive lookup",
			sql:           "SELECT U.NAME FROM users u",
			expectedCount: 0,
		},
		{
			name:          "INSERT with numeric TableRef target does not phantom-flag",
			sql:           "INSERT INTO [123 AS t] (a, b) VALUES (1, 2)",
			expectedCount: 0,
		},
		{
			name:          "FROM-subquery cannot see sibling FROM items",
			sql:           "SELECT * FROM users u, (SELECT u.id) x",
			expectedCount: 1,
		},
		{
			name:          "CTE body unknown column is flagged",
			sql:           "WITH x AS (SELECT nme FROM users) SELECT 1",
			expectedCount: 1,
		},
		{
			name:          "CTE body with UNION flags both branches",
			sql:           "WITH x AS (SELECT nme FROM users UNION SELECT eml FROM users) SELECT 1",
			expectedCount: 2,
		},
		{
			name:          "CTE body with VALUES does not error",
			sql:           "WITH x AS (VALUES (1)) SELECT 1",
			expectedCount: 0,
		},
		{
			name:          "INSERT WITH visible to source SELECT",
			sql:           "WITH ids AS (SELECT id FROM users) INSERT INTO users (id) SELECT id FROM ids",
			expectedCount: 0,
		},
	}

	cat := usersAndOrdersCatalog(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stmts, err := parser.Parse(tc.sql)
			require.NoError(t, err, "parse must succeed")

			errs := CheckColumnNames(stmts, tc.sql, cat)
			require.Len(t, errs, tc.expectedCount)

			if tc.expectedCount == 0 {
				return
			}
			first := errs[0]
			if tc.expectedCode != "" {
				require.Equal(t, tc.expectedCode, first.Code)
			}
			if tc.expectedCategory != "" {
				require.Equal(t, tc.expectedCategory, first.Category)
			}
			if tc.expectedMessage != "" {
				require.Contains(t, first.Message, tc.expectedMessage)
			}
			if tc.expectedAvail != nil {
				avail, ok := first.Context["available_columns"].([]string)
				require.True(t, ok, "available_columns must be []string")
				require.Equal(t, tc.expectedAvail, avail)
			}
		})
	}
}

// TestCheckColumnNamesPosition verifies that the reported position
// points at the first occurrence of the missing column in the SQL
// and lands on "nme" rather than the substring inside "name".
func TestCheckColumnNamesPosition(t *testing.T) {
	cat := usersAndOrdersCatalog(t)
	sql := "SELECT nme FROM users"
	stmts, err := parser.Parse(sql)
	require.NoError(t, err)

	errs := CheckColumnNames(stmts, sql, cat)
	require.Len(t, errs, 1)
	require.NotNil(t, errs[0].Position)
	require.Equal(t, 1, errs[0].Position.Line)
	require.Equal(t, 8, errs[0].Position.Column)
	require.Equal(t, 7, errs[0].Position.ByteOffset)
}

// TestCheckColumnNamesAmbiguousContext verifies that an ambiguous
// reference reports both candidate tables in Context["tables"], so
// agents can offer disambiguating suggestions. The available_columns
// key must NOT be present on ambiguous errors so agents can branch on
// shape rather than guessing which keys apply.
func TestCheckColumnNamesAmbiguousContext(t *testing.T) {
	cat := usersAndOrdersCatalog(t)
	sql := "SELECT id FROM users JOIN orders ON users.id = orders.user_id"
	stmts, err := parser.Parse(sql)
	require.NoError(t, err)

	errs := CheckColumnNames(stmts, sql, cat)
	require.Len(t, errs, 1)
	require.Equal(t, ambiguousColumnCode, errs[0].Code)
	tables, ok := errs[0].Context["tables"].([]string)
	require.True(t, ok, "tables must be []string")
	require.ElementsMatch(t, []string{"users", "orders"}, tables)
	_, hasAvail := errs[0].Context["available_columns"]
	require.False(t, hasAvail, "ambiguous_reference must not carry available_columns")
}

// TestCheckColumnNamesQualifiedMissingColumnContext verifies that a
// qualified ref against a known table with a missing column carries
// table + available_columns in Context (the per-source list, not the
// scope union).
func TestCheckColumnNamesQualifiedMissingColumnContext(t *testing.T) {
	cat := usersAndOrdersCatalog(t)
	sql := "SELECT u.nme FROM users u"
	stmts, err := parser.Parse(sql)
	require.NoError(t, err)

	errs := CheckColumnNames(stmts, sql, cat)
	require.Len(t, errs, 1)
	require.Equal(t, "u", errs[0].Context["table"])
	require.Equal(t,
		[]string{"id", "name", "email"},
		errs[0].Context["available_columns"])
}

// TestCheckColumnNamesMissingTableQualifierContext verifies the
// missing-FROM-clause-entry payload (qualifier names a source not in
// scope). The Context must include missing_table and available_tables
// so agents know which qualifier the user mistyped.
func TestCheckColumnNamesMissingTableQualifierContext(t *testing.T) {
	cat := usersAndOrdersCatalog(t)
	sql := "SELECT z.name FROM users"
	stmts, err := parser.Parse(sql)
	require.NoError(t, err)

	errs := CheckColumnNames(stmts, sql, cat)
	require.Len(t, errs, 1)
	require.Equal(t, "z", errs[0].Context["missing_table"])
	require.Equal(t, []string{"users"}, errs[0].Context["available_tables"])
}

// TestCheckColumnNamesNilCatalog verifies that a nil catalog is a
// no-op, mirroring CheckTableNames.
func TestCheckColumnNamesNilCatalog(t *testing.T) {
	stmts, err := parser.Parse("SELECT nme FROM users")
	require.NoError(t, err)
	require.Empty(t, CheckColumnNames(stmts, "SELECT nme FROM users", nil))
}

// TestCheckColumnNamesEmptyStatements verifies the no-statements path.
func TestCheckColumnNamesEmptyStatements(t *testing.T) {
	cat := usersAndOrdersCatalog(t)
	require.Empty(t, CheckColumnNames(nil, "", cat))
}
