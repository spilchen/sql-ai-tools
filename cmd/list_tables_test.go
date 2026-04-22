// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"bytes"
	"encoding/json"
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
