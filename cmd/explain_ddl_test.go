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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/output"
)

// runExplainDDL executes `crdb-sql explain-ddl` with the supplied args
// and stdin, returning the captured stdout buffer and the Execute error.
// Mirror of runExplain so the table tests below stay terse.
func runExplainDDL(t *testing.T, stdin string, args ...string) (*bytes.Buffer, error) {
	t.Helper()
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"explain-ddl"}, args...))
	return &stdout, root.Execute()
}

// TestExplainDDLCmdNoDSN verifies that explain-ddl without a DSN fails
// before any cluster contact, returning a structured error that names
// both the flag and the env var.
func TestExplainDDLCmdNoDSN(t *testing.T) {
	t.Setenv("CRDB_DSN", "")

	_, err := runExplainDDL(t, "", "-e", "ALTER TABLE t ADD COLUMN x INT")
	require.Error(t, err)
	require.ErrorContains(t, err, "no connection string")
}

// TestExplainDDLCmdNoDSNJSON verifies the JSON envelope shape when no
// DSN is configured: tier=connected, status=disconnected, an error
// entry pointing the user at --dsn / CRDB_DSN, and no Data payload.
func TestExplainDDLCmdNoDSNJSON(t *testing.T) {
	t.Setenv("CRDB_DSN", "")

	stdout, err := runExplainDDL(t, "", "--output", "json", "-e", "ALTER TABLE t ADD COLUMN x INT")
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, output.TierConnected, env.Tier)
	require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)
	require.Len(t, env.Errors, 1)
	require.Contains(t, env.Errors[0].Message, "--dsn")
	require.Contains(t, env.Errors[0].Message, "CRDB_DSN")
	require.Empty(t, env.Data)
}

// TestExplainDDLCmdNoInput verifies that an invocation with no -e, no
// file, and no piped stdin reports an empty-input error rather than
// attempting an EXPLAIN of the empty string.
func TestExplainDDLCmdNoInput(t *testing.T) {
	t.Setenv("CRDB_DSN", "postgres://envhost:26257/defaultdb")

	stdout, err := runExplainDDL(t, "", "--output", "json")
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.Contains(t, env.Errors[0].Message, "no SQL input")
}

// TestExplainDDLCmdReachesConnectAttempt verifies that with a SQL input
// and an unreachable DSN, explain-ddl gets past input parsing and DSN
// validation and fails at the connect step. The same "connect to
// CockroachDB" wording as ping/explain locks in that the conn.Manager
// path is shared.
func TestExplainDDLCmdReachesConnectAttempt(t *testing.T) {
	t.Setenv("CRDB_DSN", "")

	stdout, err := runExplainDDL(t, "", "--output", "json",
		"--dsn", "postgres://flaghost:26257/defaultdb?connect_timeout=1",
		"-e", "ALTER TABLE t ADD COLUMN x INT")
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.NotContains(t, env.Errors[0].Message, "no connection string")
	require.Contains(t, env.Errors[0].Message, "connect to CockroachDB")
}

// TestExplainDDLCmdReadsFromFileArg verifies that a positional file
// argument is plumbed through sqlinput.ReadSQL and reaches the connect
// step (rather than failing at input resolution).
func TestExplainDDLCmdReadsFromFileArg(t *testing.T) {
	t.Setenv("CRDB_DSN", "")

	dir := t.TempDir()
	sqlFile := filepath.Join(dir, "ddl.sql")
	require.NoError(t, os.WriteFile(sqlFile, []byte("ALTER TABLE t ADD COLUMN x INT"), 0644))

	stdout, err := runExplainDDL(t, "", "--output", "json",
		"--dsn", "postgres://flaghost:26257/defaultdb?connect_timeout=1",
		sqlFile)
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.Contains(t, env.Errors[0].Message, "connect to CockroachDB",
		"file arg must reach the connect step, not fail at input parsing")
}

// TestExplainDDLCmdReadsFromStdin verifies that piped SQL on stdin
// reaches the connect step.
func TestExplainDDLCmdReadsFromStdin(t *testing.T) {
	t.Setenv("CRDB_DSN", "")

	stdout, err := runExplainDDL(t, "ALTER TABLE t ADD COLUMN x INT\n", "--output", "json",
		"--dsn", "postgres://flaghost:26257/defaultdb?connect_timeout=1")
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.Contains(t, env.Errors[0].Message, "connect to CockroachDB",
		"stdin input must reach the connect step, not fail at input parsing")
}

// TestExplainDDLCmdRejectsExtraArgs verifies that more than one
// positional argument is rejected (the optional positional is the SQL
// file).
func TestExplainDDLCmdRejectsExtraArgs(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"explain-ddl", "file1.sql", "file2.sql"})

	err := root.Execute()
	require.Error(t, err)
}
