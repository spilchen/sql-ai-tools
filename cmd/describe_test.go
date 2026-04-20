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
	require.Contains(t, env.Errors[0].Message, "parse schema file")
}
