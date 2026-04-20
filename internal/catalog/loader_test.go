// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeSQL(t *testing.T, sql string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "schema.sql")
	require.NoError(t, os.WriteFile(path, []byte(sql), 0644))
	return path
}

func TestLoadFiles(t *testing.T) {
	tests := []struct {
		name      string
		sql       string
		tableName string
		wantTable Table
		wantErr   string
		wantNames []string
	}{
		{
			name: "basic columns",
			sql: `CREATE TABLE users (
				id INT8,
				name TEXT,
				active BOOL
			)`,
			tableName: "users",
			wantTable: Table{
				Name: "users",
				Columns: []Column{
					{Name: "id", Type: "INT8", Nullable: true},
					{Name: "name", Type: "STRING", Nullable: true},
					{Name: "active", Type: "BOOL", Nullable: true},
				},
				PrimaryKey: []string{},
				Indexes:    []Index{},
			},
		},
		{
			name: "inline primary key forces not null",
			sql: `CREATE TABLE items (
				id INT8 PRIMARY KEY,
				label TEXT
			)`,
			tableName: "items",
			wantTable: Table{
				Name: "items",
				Columns: []Column{
					{Name: "id", Type: "INT8", Nullable: false},
					{Name: "label", Type: "STRING", Nullable: true},
				},
				PrimaryKey: []string{"id"},
				Indexes:    []Index{},
			},
		},
		{
			name: "composite table-level primary key implies not null",
			sql: `CREATE TABLE kv (
				region TEXT,
				id INT8,
				val TEXT,
				PRIMARY KEY (region, id)
			)`,
			tableName: "kv",
			wantTable: Table{
				Name: "kv",
				Columns: []Column{
					{Name: "region", Type: "STRING", Nullable: false},
					{Name: "id", Type: "INT8", Nullable: false},
					{Name: "val", Type: "STRING", Nullable: true},
				},
				PrimaryKey: []string{"region", "id"},
				Indexes:    []Index{},
			},
		},
		{
			name: "inline unique synthesizes index name",
			sql: `CREATE TABLE accounts (
				id INT8 PRIMARY KEY,
				email TEXT UNIQUE
			)`,
			tableName: "accounts",
			wantTable: Table{
				Name: "accounts",
				Columns: []Column{
					{Name: "id", Type: "INT8", Nullable: false},
					{Name: "email", Type: "STRING", Nullable: true},
				},
				PrimaryKey: []string{"id"},
				Indexes: []Index{
					{Name: "accounts_email_key", Columns: []string{"email"}, Unique: true},
				},
			},
		},
		{
			name: "named unique constraint preserves name",
			sql: `CREATE TABLE users (
				id INT8 PRIMARY KEY,
				email TEXT CONSTRAINT uq_email UNIQUE
			)`,
			tableName: "users",
			wantTable: Table{
				Name: "users",
				Columns: []Column{
					{Name: "id", Type: "INT8", Nullable: false},
					{Name: "email", Type: "STRING", Nullable: true},
				},
				PrimaryKey: []string{"id"},
				Indexes: []Index{
					{Name: "uq_email", Columns: []string{"email"}, Unique: true},
				},
			},
		},
		{
			name: "table-level unique constraint",
			sql: `CREATE TABLE orders (
				store_id INT8,
				order_num INT8,
				UNIQUE (store_id, order_num)
			)`,
			tableName: "orders",
			wantTable: Table{
				Name: "orders",
				Columns: []Column{
					{Name: "store_id", Type: "INT8", Nullable: true},
					{Name: "order_num", Type: "INT8", Nullable: true},
				},
				PrimaryKey: []string{},
				Indexes: []Index{
					{Name: "", Columns: []string{"store_id", "order_num"}, Unique: true},
				},
			},
		},
		{
			name: "explicit index",
			sql: `CREATE TABLE events (
				id INT8 PRIMARY KEY,
				ts TIMESTAMPTZ,
				INDEX idx_ts (ts)
			)`,
			tableName: "events",
			wantTable: Table{
				Name: "events",
				Columns: []Column{
					{Name: "id", Type: "INT8", Nullable: false},
					{Name: "ts", Type: "TIMESTAMPTZ", Nullable: true},
				},
				PrimaryKey: []string{"id"},
				Indexes: []Index{
					{Name: "idx_ts", Columns: []string{"ts"}, Unique: false},
				},
			},
		},
		{
			name: "default expressions",
			sql: `CREATE TABLE records (
				id INT8 PRIMARY KEY,
				count INT8 DEFAULT 0,
				created_at TIMESTAMPTZ DEFAULT now()
			)`,
			tableName: "records",
			wantTable: Table{
				Name: "records",
				Columns: []Column{
					{Name: "id", Type: "INT8", Nullable: false},
					{Name: "count", Type: "INT8", Nullable: true, Default: strPtr("0")},
					{Name: "created_at", Type: "TIMESTAMPTZ", Nullable: true, Default: strPtr("now()")},
				},
				PrimaryKey: []string{"id"},
				Indexes:    []Index{},
			},
		},
		{
			name: "explicit null and not null",
			sql: `CREATE TABLE t (
				a INT8 NULL,
				b INT8 NOT NULL
			)`,
			tableName: "t",
			wantTable: Table{
				Name: "t",
				Columns: []Column{
					{Name: "a", Type: "INT8", Nullable: true},
					{Name: "b", Type: "INT8", Nullable: false},
				},
				PrimaryKey: []string{},
				Indexes:    []Index{},
			},
		},
		{
			name: "non-create-table statements skipped",
			sql: `
				SELECT 1;
				ALTER TABLE foo ADD COLUMN bar INT8;
				CREATE TABLE t (id INT8 PRIMARY KEY);
				INSERT INTO t VALUES (1);
			`,
			tableName: "t",
			wantTable: Table{
				Name: "t",
				Columns: []Column{
					{Name: "id", Type: "INT8", Nullable: false},
				},
				PrimaryKey: []string{"id"},
				Indexes:    []Index{},
			},
		},
		{
			name: "multiple tables in one file",
			sql: `
				CREATE TABLE a (id INT8 PRIMARY KEY);
				CREATE TABLE b (id INT8 PRIMARY KEY);
			`,
			tableName: "b",
			wantNames: []string{"a", "b"},
			wantTable: Table{
				Name: "b",
				Columns: []Column{
					{Name: "id", Type: "INT8", Nullable: false},
				},
				PrimaryKey: []string{"id"},
				Indexes:    []Index{},
			},
		},
		{
			name:    "empty file",
			sql:     "",
			wantErr: "",
		},
		{
			name:    "parse error",
			sql:     "CREAT TABLE bad (id INT8);",
			wantErr: "parse schema file",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeSQL(t, tc.sql)
			cat, err := LoadFiles([]string{path})

			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)

			if tc.wantNames != nil {
				require.Equal(t, tc.wantNames, cat.TableNames())
			}

			if tc.tableName == "" {
				require.Empty(t, cat.TableNames())
				return
			}

			tbl, ok := cat.Table(tc.tableName)
			require.True(t, ok, "table %q not found in catalog", tc.tableName)
			require.Equal(t, tc.wantTable, tbl)
		})
	}
}

func TestLoadFilesMultipleFiles(t *testing.T) {
	path1 := writeSQL(t, `CREATE TABLE a (id INT8 PRIMARY KEY)`)
	path2 := writeSQL(t, `CREATE TABLE b (id INT8 PRIMARY KEY)`)

	cat, err := LoadFiles([]string{path1, path2})
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, cat.TableNames())

	_, ok := cat.Table("a")
	require.True(t, ok)
	_, ok = cat.Table("b")
	require.True(t, ok)
}

func TestLoadFilesDuplicateTableLastWins(t *testing.T) {
	path1 := writeSQL(t, `CREATE TABLE t (old_col INT8)`)
	path2 := writeSQL(t, `CREATE TABLE t (new_col TEXT)`)

	cat, err := LoadFiles([]string{path1, path2})
	require.NoError(t, err)
	require.Equal(t, []string{"t"}, cat.TableNames())

	tbl, ok := cat.Table("t")
	require.True(t, ok)
	require.Equal(t, "new_col", tbl.Columns[0].Name)

	require.Len(t, cat.Warnings(), 1)
	require.Contains(t, cat.Warnings()[0], "defined more than once")
}

func TestLoadFilesFileNotFound(t *testing.T) {
	_, err := LoadFiles([]string{"/nonexistent/schema.sql"})
	require.ErrorContains(t, err, "read schema file")
}

func TestLoadFilesCaseInsensitiveLookup(t *testing.T) {
	path := writeSQL(t, `CREATE TABLE Users (id INT8 PRIMARY KEY)`)
	cat, err := LoadFiles([]string{path})
	require.NoError(t, err)

	_, ok := cat.Table("users")
	require.True(t, ok)
	_, ok = cat.Table("USERS")
	require.True(t, ok)
	_, ok = cat.Table("Users")
	require.True(t, ok)
}

func TestLoadFilesSkippedStatementsWarning(t *testing.T) {
	path := writeSQL(t, `
		SELECT 1;
		CREATE TABLE t (id INT8 PRIMARY KEY);
		ALTER TABLE t ADD COLUMN name STRING;
	`)

	cat, err := LoadFiles([]string{path})
	require.NoError(t, err)
	require.Equal(t, []string{"t"}, cat.TableNames())

	require.Len(t, cat.Warnings(), 1)
	require.Contains(t, cat.Warnings()[0], "skipped 2 non-CREATE TABLE")
	require.Contains(t, cat.Warnings()[0], "SELECT")
	require.Contains(t, cat.Warnings()[0], "ALTER TABLE")
}

func TestLoadFilesNoWarningsWhenAllCreateTable(t *testing.T) {
	path := writeSQL(t, `CREATE TABLE t (id INT8 PRIMARY KEY)`)
	cat, err := LoadFiles([]string{path})
	require.NoError(t, err)
	require.Empty(t, cat.Warnings())
}

func TestLoadFilesFileTooLarge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.sql")

	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(maxSchemaFileSize+1))
	require.NoError(t, f.Close())

	_, err = LoadFiles([]string{path})
	require.ErrorContains(t, err, "too large")
}

func strPtr(s string) *string {
	return &s
}
