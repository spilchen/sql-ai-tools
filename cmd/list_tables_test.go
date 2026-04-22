// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/output"
)

func TestListTablesCmdTextOutput(t *testing.T) {
	schema := writeSchemaFile(t, `
		CREATE TABLE users (id INT8 PRIMARY KEY);
		CREATE TABLE orders (id INT8 PRIMARY KEY);
	`)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"list-tables", "--schema", schema})

	require.NoError(t, root.Execute())

	got := stdout.String()
	require.Equal(t, "users\norders\n", got)
}

func TestListTablesCmdJSONOutput(t *testing.T) {
	schema := writeSchemaFile(t, `
		CREATE TABLE users (id INT8 PRIMARY KEY);
		CREATE TABLE orders (id INT8 PRIMARY KEY);
	`)

	root := newRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"list-tables", "--schema", schema, "--output", "json"})

	require.NoError(t, root.Execute())
	require.Empty(t, stderr.String())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, output.TierSchemaFile, env.Tier)
	require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)
	require.NotEmpty(t, env.ParserVersion)
	require.Empty(t, env.Errors)

	var result listTablesResult
	require.NoError(t, json.Unmarshal(env.Data, &result))
	require.Equal(t, []string{"users", "orders"}, result.Tables)
}

func TestListTablesCmdMissingSchemaFlag(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"list-tables", "--output", "json"})

	err := root.Execute()
	require.Error(t, err)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.NotEmpty(t, env.Errors)
	require.Contains(t, env.Errors[0].Message, "--schema")
	// The message also nudges users toward the config-driven path so
	// they discover the alternative without consulting docs.
	require.Contains(t, env.Errors[0].Message, "crdb-sql.yaml")
}

func TestListTablesCmdEmptySchema(t *testing.T) {
	schema := writeSchemaFile(t, `SELECT 1;`)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"list-tables", "--schema", schema, "--output", "json"})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))

	var result listTablesResult
	require.NoError(t, json.Unmarshal(env.Data, &result))
	require.Empty(t, result.Tables)

	require.NotEmpty(t, env.Errors)
	require.Equal(t, output.SeverityWarning, env.Errors[0].Severity)
}

func TestListTablesCmdMultipleSchemaFiles(t *testing.T) {
	schema1 := writeSchemaFile(t, `CREATE TABLE a (id INT8 PRIMARY KEY)`)
	schema2 := writeSchemaFile(t, `CREATE TABLE b (id INT8 PRIMARY KEY)`)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"list-tables", "--schema", schema1, "--schema", schema2, "--output", "json"})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Empty(t, env.Errors)

	var result listTablesResult
	require.NoError(t, json.Unmarshal(env.Data, &result))
	require.Equal(t, []string{"a", "b"}, result.Tables)
}

func TestListTablesCmdWarningsPropagated(t *testing.T) {
	schema := writeSchemaFile(t, `
		SELECT 1;
		CREATE TABLE t (id INT8 PRIMARY KEY);
	`)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"list-tables", "--schema", schema, "--output", "json"})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.NotNil(t, env.Data)

	require.Len(t, env.Errors, 1)
	require.Equal(t, output.SeverityWarning, env.Errors[0].Severity)
	require.Contains(t, env.Errors[0].Message, "skipped 1 non-CREATE TABLE")

	var result listTablesResult
	require.NoError(t, json.Unmarshal(env.Data, &result))
	require.Equal(t, []string{"t"}, result.Tables)
}

func TestListTablesCmdParseError(t *testing.T) {
	schema := writeSchemaFile(t, `CREAT TABLE bad (id INT8)`)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"list-tables", "--schema", schema, "--output", "json"})

	err := root.Execute()
	require.Error(t, err)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.NotEmpty(t, env.Errors)
	require.Contains(t, env.Errors[0].Message, "parse schema")
}

// setupListTablesConfigProject lays down a tiny project with two
// schema files and a crdb-sql.yaml that picks them up by glob.
// Mirrors setupDescribeConfigProject. Queries are intentionally
// absent — list-tables doesn't read them.
func setupListTablesConfigProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "schema/users.sql",
		"CREATE TABLE users (id INT8 PRIMARY KEY);\n")
	writeFile(t, dir, "schema/orders.sql",
		"CREATE TABLE orders (id INT8 PRIMARY KEY);\n")
	writeFile(t, dir, "crdb-sql.yaml", `version: 1
sql:
  - schema: ["schema/*.sql"]
    queries: []
`)
	return dir
}

func TestListTablesCmdConfigText(t *testing.T) {
	dir := setupListTablesConfigProject(t)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"list-tables",
		"--config", filepath.Join(dir, "crdb-sql.yaml"),
	})

	require.NoError(t, root.Execute())

	// expandConfigSchemaPaths sorts paths, so orders.sql is processed
	// before users.sql; TableNames preserves encounter order.
	require.Equal(t, "orders\nusers\n", stdout.String())
}

func TestListTablesCmdConfigJSON(t *testing.T) {
	dir := setupListTablesConfigProject(t)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"list-tables",
		"--output", "json",
		"--config", filepath.Join(dir, "crdb-sql.yaml"),
	})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, output.TierSchemaFile, env.Tier)
	require.Empty(t, env.Errors)

	var result listTablesResult
	require.NoError(t, json.Unmarshal(env.Data, &result))
	require.Equal(t, []string{"orders", "users"}, result.Tables)
}

// TestListTablesCmdConfigEmptyExpansion covers the case where a
// crdb-sql.yaml is discovered but its globs match no files. The error
// must name the config (not the no-config message) so users can tell
// their config was loaded and the globs are the problem.
func TestListTablesCmdConfigEmptyExpansion(t *testing.T) {
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
		"list-tables",
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

// TestListTablesCmdExplicitSchemaWinsOverConfig confirms there is no
// merging: when --schema is passed alongside --config, only the
// flag-supplied file is loaded. Config has users+orders; --schema
// points at a file with only "products".
func TestListTablesCmdExplicitSchemaWinsOverConfig(t *testing.T) {
	dir := setupListTablesConfigProject(t)
	override := writeSchemaFile(t, `CREATE TABLE products (id INT8 PRIMARY KEY)`)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"list-tables",
		"--config", filepath.Join(dir, "crdb-sql.yaml"),
		"--schema", override,
		"--output", "json",
	})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Empty(t, env.Errors)

	var result listTablesResult
	require.NoError(t, json.Unmarshal(env.Data, &result))
	require.Equal(t, []string{"products"}, result.Tables)
}

// TestListTablesCmdConfigMultiPairDedup exercises the cross-pair
// deduplication in expandConfigSchemaPaths. Two pairs share the same
// schema glob; without the outer dedup map, the shared file would be
// loaded twice and catalog.LoadFiles would surface a duplicate-table
// error.
func TestListTablesCmdConfigMultiPairDedup(t *testing.T) {
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
		"list-tables",
		"--output", "json",
		"--config", filepath.Join(dir, "crdb-sql.yaml"),
	})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Empty(t, env.Errors, "duplicate-table errors indicate dedup failed")

	var result listTablesResult
	require.NoError(t, json.Unmarshal(env.Data, &result))
	require.Equal(t, []string{"users"}, result.Tables)
}

// TestListTablesCmdConfigBadGlob covers the err-return inside
// expandConfigSchemaPaths' loop. A malformed pattern (unmatched `[`)
// makes doublestar.FilepathGlob return ErrBadPattern; the rendered
// envelope must surface that as an error rather than silently
// producing an empty path list.
func TestListTablesCmdConfigBadGlob(t *testing.T) {
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
		"list-tables",
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

// TestListTablesCmdConfigWarningsPropagated guards the
// appendSchemaWarnings call inside renderListTables on the
// config-fallback branch. TestListTablesCmdWarningsPropagated already
// covers the explicit --schema branch; this one covers the config
// branch independently so a future refactor that splits the warning
// hookup loses coverage on at most one path, not both.
func TestListTablesCmdConfigWarningsPropagated(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "schema/mixed.sql", `
		SELECT 1;
		CREATE TABLE t (id INT8 PRIMARY KEY);
	`)
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
		"list-tables",
		"--output", "json",
		"--config", filepath.Join(dir, "crdb-sql.yaml"),
	})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))

	require.Len(t, env.Errors, 1)
	require.Equal(t, output.SeverityWarning, env.Errors[0].Severity)
	require.Contains(t, env.Errors[0].Message, "skipped 1 non-CREATE TABLE")

	var result listTablesResult
	require.NoError(t, json.Unmarshal(env.Data, &result))
	require.Equal(t, []string{"t"}, result.Tables)
}

// TestListTablesCmdConfigSchemaParseError covers the
// renderSchemaLoadError path in the config branch: a glob-matched file
// with malformed DDL must produce a parse-error envelope.
func TestListTablesCmdConfigSchemaParseError(t *testing.T) {
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
		"list-tables",
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
