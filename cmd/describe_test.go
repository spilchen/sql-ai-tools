// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/catalog"
	"github.com/spilchen/sql-ai-tools/internal/output"
)

func writeSchemaFile(t *testing.T, sql string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "schema.sql")
	require.NoError(t, os.WriteFile(path, []byte(sql), 0644))
	return path
}

const testSchema = `
CREATE TABLE users (
	id INT8 PRIMARY KEY,
	email VARCHAR(255) NOT NULL UNIQUE,
	name STRING,
	created_at TIMESTAMPTZ DEFAULT now()
);
`

func TestDescribeCmdTextOutput(t *testing.T) {
	schema := writeSchemaFile(t, testSchema)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"describe", "users", "--schema", schema})

	require.NoError(t, root.Execute())

	got := stdout.String()
	require.Contains(t, got, "Table: users")
	require.Contains(t, got, "id")
	require.Contains(t, got, "INT8")
	require.Contains(t, got, "email")
	require.Contains(t, got, "VARCHAR(255)")
	require.Contains(t, got, "Primary Key: id")
	require.Contains(t, got, "users_email_key")
}

func TestDescribeCmdJSONOutput(t *testing.T) {
	schema := writeSchemaFile(t, testSchema)

	root := newRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"describe", "users", "--schema", schema, "--output", "json"})

	require.NoError(t, root.Execute())
	require.Empty(t, stderr.String())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, output.TierSchemaFile, env.Tier)
	require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)
	require.NotEmpty(t, env.ParserVersion)
	require.Empty(t, env.Errors)

	var tbl catalog.Table
	require.NoError(t, json.Unmarshal(env.Data, &tbl))
	require.Equal(t, "users", tbl.Name)
	require.Len(t, tbl.Columns, 4)
	require.Equal(t, []string{"id"}, tbl.PrimaryKey)
}

func TestDescribeCmdJSONColumns(t *testing.T) {
	schema := writeSchemaFile(t, testSchema)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"describe", "users", "--schema", schema, "--output", "json"})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))

	var tbl catalog.Table
	require.NoError(t, json.Unmarshal(env.Data, &tbl))

	id := tbl.Columns[0]
	require.Equal(t, "id", id.Name)
	require.Equal(t, "INT8", id.Type)
	require.False(t, id.Nullable)
	require.Nil(t, id.Default)

	email := tbl.Columns[1]
	require.Equal(t, "email", email.Name)
	require.Equal(t, "VARCHAR(255)", email.Type)
	require.False(t, email.Nullable)

	name := tbl.Columns[2]
	require.Equal(t, "name", name.Name)
	require.True(t, name.Nullable)

	createdAt := tbl.Columns[3]
	require.Equal(t, "created_at", createdAt.Name)
	require.NotNil(t, createdAt.Default)
	require.Equal(t, "now()", *createdAt.Default)
}

func TestDescribeCmdJSONIndexes(t *testing.T) {
	schema := writeSchemaFile(t, testSchema)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"describe", "users", "--schema", schema, "--output", "json"})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))

	var tbl catalog.Table
	require.NoError(t, json.Unmarshal(env.Data, &tbl))

	require.Len(t, tbl.Indexes, 1)
	require.Equal(t, "users_email_key", tbl.Indexes[0].Name)
	require.Equal(t, []string{"email"}, tbl.Indexes[0].Columns)
	require.True(t, tbl.Indexes[0].Unique)
}

func TestDescribeCmdMissingSchemaFlag(t *testing.T) {
	// Run from a directory with no crdb-sql.yaml so the config branch
	// is inert and the explicit-schema requirement applies.
	t.Chdir(t.TempDir())

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"describe", "users", "--output", "json"})

	err := root.Execute()
	require.Error(t, err)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.NotEmpty(t, env.Errors)
	require.Contains(t, env.Errors[0].Message, "--schema")
	require.Contains(t, env.Errors[0].Message, "crdb-sql.yaml")
}

func TestDescribeCmdTableNotFound(t *testing.T) {
	schema := writeSchemaFile(t, `CREATE TABLE orders (id INT8 PRIMARY KEY)`)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"describe", "users", "--schema", schema, "--output", "json"})

	err := root.Execute()
	require.Error(t, err)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.NotEmpty(t, env.Errors)
	require.Contains(t, env.Errors[0].Message, "users")
	require.Contains(t, env.Errors[0].Message, "orders")
}

func TestDescribeCmdMultipleSchemaFiles(t *testing.T) {
	schema1 := writeSchemaFile(t, `CREATE TABLE a (id INT8 PRIMARY KEY)`)
	schema2 := writeSchemaFile(t, `CREATE TABLE b (id INT8 PRIMARY KEY)`)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"describe", "b", "--schema", schema1, "--schema", schema2, "--output", "json"})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Empty(t, env.Errors)

	var tbl catalog.Table
	require.NoError(t, json.Unmarshal(env.Data, &tbl))
	require.Equal(t, "b", tbl.Name)
}

func TestDescribeCmdPrimaryKeyImpliesNotNull(t *testing.T) {
	schema := writeSchemaFile(t, `CREATE TABLE t (id INT8 PRIMARY KEY)`)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"describe", "t", "--schema", schema, "--output", "json"})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))

	var tbl catalog.Table
	require.NoError(t, json.Unmarshal(env.Data, &tbl))
	require.False(t, tbl.Columns[0].Nullable)
}

func TestDescribeCmdInlineUniqueSynthesizesName(t *testing.T) {
	schema := writeSchemaFile(t, `CREATE TABLE t (id INT8 PRIMARY KEY, code STRING UNIQUE)`)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"describe", "t", "--schema", schema, "--output", "json"})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))

	var tbl catalog.Table
	require.NoError(t, json.Unmarshal(env.Data, &tbl))
	require.Len(t, tbl.Indexes, 1)
	require.Equal(t, "t_code_key", tbl.Indexes[0].Name)
}

func TestDescribeCmdWarningsInJSONOutput(t *testing.T) {
	schema := writeSchemaFile(t, `
		SELECT 1;
		CREATE TABLE t (id INT8 PRIMARY KEY);
	`)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"describe", "t", "--schema", schema, "--output", "json"})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.NotNil(t, env.Data)

	require.Len(t, env.Errors, 1)
	require.Equal(t, output.SeverityWarning, env.Errors[0].Severity)
	require.Contains(t, env.Errors[0].Message, "skipped 1 non-CREATE TABLE")
}

func TestDescribeCmdStdinBasic(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(bytes.NewBufferString(`CREATE TABLE t (id INT8 PRIMARY KEY, name TEXT)`))
	root.SetArgs([]string{"describe", "t", "--schema", "-", "--output", "json"})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Empty(t, env.Errors)

	var tbl catalog.Table
	require.NoError(t, json.Unmarshal(env.Data, &tbl))
	require.Equal(t, "t", tbl.Name)
	require.Len(t, tbl.Columns, 2)
	require.Equal(t, []string{"id"}, tbl.PrimaryKey)
}

func TestDescribeCmdStdinTextOutput(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(bytes.NewBufferString(`CREATE TABLE users (id INT8 PRIMARY KEY, email TEXT NOT NULL)`))
	root.SetArgs([]string{"describe", "users", "--schema", "-"})

	require.NoError(t, root.Execute())

	got := stdout.String()
	require.Contains(t, got, "Table: users")
	require.Contains(t, got, "id")
	require.Contains(t, got, "email")
	require.Contains(t, got, "Primary Key: id")
}

func TestDescribeCmdStdinMixedWithFile(t *testing.T) {
	schema := writeSchemaFile(t, `CREATE TABLE a (id INT8 PRIMARY KEY)`)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(bytes.NewBufferString(`CREATE TABLE b (id INT8 PRIMARY KEY)`))
	root.SetArgs([]string{"describe", "b", "--schema", schema, "--schema", "-", "--output", "json"})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Empty(t, env.Errors)

	var tbl catalog.Table
	require.NoError(t, json.Unmarshal(env.Data, &tbl))
	require.Equal(t, "b", tbl.Name)
}

func TestDescribeCmdStdinDuplicateRejected(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(bytes.NewBufferString(`CREATE TABLE t (id INT8 PRIMARY KEY)`))
	root.SetArgs([]string{"describe", "t", "--schema", "-", "--schema", "-", "--output", "json"})

	err := root.Execute()
	require.Error(t, err)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.NotEmpty(t, env.Errors)
	require.Contains(t, env.Errors[0].Message, "stdin")
	require.Contains(t, env.Errors[0].Message, "once")
}

func TestDescribeCmdStdinEmpty(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(bytes.NewBufferString(""))
	root.SetArgs([]string{"describe", "t", "--schema", "-", "--output", "json"})

	err := root.Execute()
	require.Error(t, err)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 2)
	require.Equal(t, output.SeverityWarning, env.Errors[0].Severity)
	require.Contains(t, env.Errors[0].Message, "no SQL statements")
	require.Contains(t, env.Errors[1].Message, "not found")
}

func TestDescribeCmdParseError(t *testing.T) {
	schema := writeSchemaFile(t, `CREAT TABLE bad (id INT8)`)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"describe", "bad", "--schema", schema, "--output", "json"})

	err := root.Execute()
	require.Error(t, err)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.NotEmpty(t, env.Errors)
	require.Contains(t, env.Errors[0].Message, "parse schema")
}

// setupDescribeConfigProject lays down a tiny project with one schema
// file and a crdb-sql.yaml that picks it up by glob. Mirrors
// setupValidateConfigProject. Queries are intentionally absent —
// describe doesn't read them.
func setupDescribeConfigProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "schema/users.sql",
		"CREATE TABLE users (id INT8 PRIMARY KEY, email VARCHAR(255) NOT NULL);\n")
	writeFile(t, dir, "crdb-sql.yaml", `version: 1
sql:
  - schema: ["schema/*.sql"]
    queries: []
`)
	return dir
}

func TestDescribeCmdConfigText(t *testing.T) {
	dir := setupDescribeConfigProject(t)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"describe", "users",
		"--config", filepath.Join(dir, "crdb-sql.yaml"),
	})

	require.NoError(t, root.Execute())

	got := stdout.String()
	require.Contains(t, got, "Table: users")
	require.Contains(t, got, "email")
	require.Contains(t, got, "Primary Key: id")
}

func TestDescribeCmdConfigJSON(t *testing.T) {
	dir := setupDescribeConfigProject(t)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"describe", "users",
		"--output", "json",
		"--config", filepath.Join(dir, "crdb-sql.yaml"),
	})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, output.TierSchemaFile, env.Tier)
	require.Empty(t, env.Errors)

	var tbl catalog.Table
	require.NoError(t, json.Unmarshal(env.Data, &tbl))
	require.Equal(t, "users", tbl.Name)
	require.Len(t, tbl.Columns, 2)
	require.Equal(t, []string{"id"}, tbl.PrimaryKey)
}

func TestDescribeCmdConfigTableNotFound(t *testing.T) {
	dir := setupDescribeConfigProject(t)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"describe", "nope",
		"--output", "json",
		"--config", filepath.Join(dir, "crdb-sql.yaml"),
	})

	err := root.Execute()
	require.Error(t, err)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.NotEmpty(t, env.Errors)
	require.Contains(t, env.Errors[0].Message, `"nope" not found`)
	require.Contains(t, env.Errors[0].Message, "users")
}

func TestDescribeCmdConfigEmptyExpansion(t *testing.T) {
	// Config is loaded but its globs match nothing. The error must name
	// the config (not the no-config message) so users can tell their
	// config was discovered and the globs are the problem.
	dir := t.TempDir()
	writeFile(t, dir, "crdb-sql.yaml", `version: 1
sql:
  - schema: ["schema/*.sql"]
    queries: []
`)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"describe", "users",
		"--output", "json",
		"--config", filepath.Join(dir, "crdb-sql.yaml"),
	})

	err := root.Execute()
	require.Error(t, err)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.NotEmpty(t, env.Errors)
	require.Contains(t, env.Errors[0].Message, "crdb-sql.yaml")
	require.Contains(t, env.Errors[0].Message, "no schema files matching")
	require.Contains(t, env.Errors[0].Message, dir)
}

func TestDescribeCmdExplicitSchemaWinsOverConfig(t *testing.T) {
	// Config points at a schema with table "users"; --schema points
	// at a different file with table "orders". The explicit flag must
	// win outright (no merging), so describing "users" should fail
	// with table-not-found.
	dir := setupDescribeConfigProject(t)
	override := writeSchemaFile(t, `CREATE TABLE orders (id INT8 PRIMARY KEY)`)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"describe", "users",
		"--config", filepath.Join(dir, "crdb-sql.yaml"),
		"--schema", override,
		"--output", "json",
	})

	err := root.Execute()
	require.Error(t, err)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.NotEmpty(t, env.Errors)
	require.Contains(t, env.Errors[0].Message, `"users" not found`)
	require.Contains(t, env.Errors[0].Message, "orders")
}

// TestDescribeCmdConfigMultiPairDedup is the only test that exercises
// the cross-pair deduplication in expandConfigSchemaPaths. Two pairs
// share the same schema glob; without the outer dedup map, the shared
// file would be loaded twice and catalog.LoadFiles would surface a
// duplicate-table error.
func TestDescribeCmdConfigMultiPairDedup(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "schema/users.sql",
		"CREATE TABLE users (id INT8 PRIMARY KEY);\n")
	writeFile(t, dir, "crdb-sql.yaml", `version: 1
sql:
  - schema: ["schema/*.sql"]
    queries: []
  - schema: ["schema/users.sql"]
    queries: []
`)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"describe", "users",
		"--output", "json",
		"--config", filepath.Join(dir, "crdb-sql.yaml"),
	})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Empty(t, env.Errors, "duplicate-table errors indicate dedup failed")

	var tbl catalog.Table
	require.NoError(t, json.Unmarshal(env.Data, &tbl))
	require.Equal(t, "users", tbl.Name)
}

// TestDescribeCmdConfigBadGlob covers the err-return inside
// expandConfigSchemaPaths' loop. A malformed pattern (unmatched `[`)
// makes doublestar.FilepathGlob return ErrBadPattern; the rendered
// envelope must surface that as an error rather than silently
// producing an empty path list.
func TestDescribeCmdConfigBadGlob(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "crdb-sql.yaml", `version: 1
sql:
  - schema: ["schema/["]
    queries: []
`)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"describe", "users",
		"--output", "json",
		"--config", filepath.Join(dir, "crdb-sql.yaml"),
	})

	err := root.Execute()
	require.Error(t, err)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.NotEmpty(t, env.Errors)
	require.Contains(t, env.Errors[0].Message, "expand glob")
}

// TestDescribeCmdConfigSchemaParseError covers the renderSchemaLoadError
// path in the config branch: a glob-matched file with malformed DDL must
// produce a parse-error envelope, not a "table not found".
func TestDescribeCmdConfigSchemaParseError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "schema/bad.sql", "CREAT TABLE oops (id INT8)")
	writeFile(t, dir, "crdb-sql.yaml", `version: 1
sql:
  - schema: ["schema/*.sql"]
    queries: []
`)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"describe", "oops",
		"--output", "json",
		"--config", filepath.Join(dir, "crdb-sql.yaml"),
	})

	err := root.Execute()
	require.Error(t, err)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.NotEmpty(t, env.Errors)
	require.Contains(t, env.Errors[0].Message, "parse schema")
}
