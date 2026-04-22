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

// runExplain executes `crdb-sql explain` with the supplied args and
// stdin, returning the captured stdout buffer and the Execute error.
// Tests use this to keep setup boilerplate out of the table.
func runExplain(t *testing.T, stdin string, args ...string) (*bytes.Buffer, error) {
	t.Helper()
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"explain"}, args...))
	return &stdout, root.Execute()
}

// TestExplainCmdNoDSN verifies that explain without a DSN fails
// before any cluster contact, returning a structured error that names
// both the flag and the env var.
func TestExplainCmdNoDSN(t *testing.T) {
	t.Setenv("CRDB_DSN", "")

	_, err := runExplain(t, "", "-e", "SELECT 1")
	require.Error(t, err)
	require.ErrorContains(t, err, "no connection string")
}

// TestExplainCmdNoDSNJSON verifies the JSON envelope shape when no DSN
// is configured: tier=connected, status=disconnected, an error entry
// pointing the user at --dsn / CRDB_DSN, and no Data payload.
func TestExplainCmdNoDSNJSON(t *testing.T) {
	t.Setenv("CRDB_DSN", "")

	stdout, err := runExplain(t, "", "--output", "json", "-e", "SELECT 1")
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

// TestExplainCmdNoInput verifies that an invocation with no -e, no
// file, and no piped stdin reports an empty-input error rather than
// attempting an EXPLAIN of the empty string.
func TestExplainCmdNoInput(t *testing.T) {
	t.Setenv("CRDB_DSN", "postgres://envhost:26257/defaultdb")

	stdout, err := runExplain(t, "", "--output", "json")
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	// The exact message comes from sqlinput.ReadSQL; assert the salient
	// substring rather than the full string to avoid coupling to its
	// wording.
	require.Contains(t, env.Errors[0].Message, "no SQL input")
}

// TestExplainCmdReachesConnectAttempt verifies that with a SQL input
// and an unreachable DSN, explain gets past input parsing and DSN
// validation and fails at the connect step. The exact error mirrors
// ping's "connect to CockroachDB" wording so this also locks in that
// the same conn.Manager path is used.
func TestExplainCmdReachesConnectAttempt(t *testing.T) {
	t.Setenv("CRDB_DSN", "")

	stdout, err := runExplain(t, "", "--output", "json",
		"--dsn", "postgres://flaghost:26257/defaultdb?connect_timeout=1",
		"-e", "SELECT 1")
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.NotContains(t, env.Errors[0].Message, "no connection string")
	require.Contains(t, env.Errors[0].Message, "connect to CockroachDB")
}

// TestExplainCmdReadsFromFileArg verifies that a positional file
// argument is plumbed through sqlinput.ReadSQL and reaches the connect
// step (rather than failing at input resolution). Asserting the same
// "connect to CockroachDB" error that the -e path produces locks in
// that file input takes the same code path.
func TestExplainCmdReadsFromFileArg(t *testing.T) {
	t.Setenv("CRDB_DSN", "")

	dir := t.TempDir()
	sqlFile := filepath.Join(dir, "q.sql")
	require.NoError(t, os.WriteFile(sqlFile, []byte("SELECT 1"), 0644))

	stdout, err := runExplain(t, "", "--output", "json",
		"--dsn", "postgres://flaghost:26257/defaultdb?connect_timeout=1",
		sqlFile)
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.Contains(t, env.Errors[0].Message, "connect to CockroachDB",
		"file arg must reach the connect step, not fail at input parsing")
}

// TestExplainCmdReadsFromStdin verifies that piped SQL on stdin reaches
// the connect step. Same shape as TestExplainCmdReadsFromFileArg but
// covers the third documented input mode.
func TestExplainCmdReadsFromStdin(t *testing.T) {
	t.Setenv("CRDB_DSN", "")

	stdout, err := runExplain(t, "SELECT 1\n", "--output", "json",
		"--dsn", "postgres://flaghost:26257/defaultdb?connect_timeout=1")
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.Contains(t, env.Errors[0].Message, "connect to CockroachDB",
		"stdin input must reach the connect step, not fail at input parsing")
}

// TestExplainCmdRejectsExtraArgs verifies that more than one positional
// argument is rejected (the optional positional is the SQL file).
func TestExplainCmdRejectsExtraArgs(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"explain", "file1.sql", "file2.sql"})

	err := root.Execute()
	require.Error(t, err)
}
