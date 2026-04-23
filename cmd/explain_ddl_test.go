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

// TestExplainDDLCmdReachesSafetyCheck verifies that with a SQL input
// and a valid DSN, explain-ddl gets past input parsing and DSN
// validation and the safety allowlist intercepts the DDL before any
// cluster contact. The default --mode=read_only rejects every DDL
// (since DDL modifies schema), so a safety_violation envelope is the
// expected stop point. The cluster is never reached, so no connect
// error is produced.
func TestExplainDDLCmdReachesSafetyCheck(t *testing.T) {
	t.Setenv("CRDB_DSN", "")

	stdout, err := runExplainDDL(t, "", "--output", "json",
		"--dsn", "postgres://flaghost:26257/defaultdb?connect_timeout=1",
		"-e", "ALTER TABLE t ADD COLUMN x INT")
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus,
		"safety rejection must short-circuit before any cluster contact")
	require.Len(t, env.Errors, 1)
	require.Equal(t, output.CodeSafetyViolation, env.Errors[0].Code)
	require.Equal(t, "ALTER TABLE", env.Errors[0].Context["tag"])
	require.Equal(t, "read_only", env.Errors[0].Context["mode"])
}

// TestExplainDDLCmdReadsFromFileArg verifies that a positional file
// argument is plumbed through sqlinput.ReadSQL and reaches the safety
// check (rather than failing at input resolution). A safety_violation
// for the file's ALTER TABLE proves the file was read and parsed.
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
	require.Equal(t, output.CodeSafetyViolation, env.Errors[0].Code,
		"file arg must reach the safety check, not fail at input parsing")
	require.Equal(t, "ALTER TABLE", env.Errors[0].Context["tag"])
}

// TestExplainDDLCmdReadsFromStdin verifies that piped SQL on stdin
// reaches the safety check. Same pattern as the file-arg test above.
func TestExplainDDLCmdReadsFromStdin(t *testing.T) {
	t.Setenv("CRDB_DSN", "")

	stdout, err := runExplainDDL(t, "ALTER TABLE t ADD COLUMN x INT\n", "--output", "json",
		"--dsn", "postgres://flaghost:26257/defaultdb?connect_timeout=1")
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.Equal(t, output.CodeSafetyViolation, env.Errors[0].Code,
		"stdin input must reach the safety check, not fail at input parsing")
	require.Equal(t, "ALTER TABLE", env.Errors[0].Context["tag"])
}

// TestExplainDDLCmdSafetyRejectsNonDDL verifies that under read_only,
// explain-ddl rejects a non-DDL inner statement with the
// "explain_ddl requires a DDL statement" reason — distinct from the
// "modifies schema" rejection that fires for DDL inputs. The two reject
// reasons differ; this test pins the SELECT case so the distinction
// cannot regress to a single generic error.
func TestExplainDDLCmdSafetyRejectsNonDDL(t *testing.T) {
	t.Setenv("CRDB_DSN", "")
	stdout, err := runExplainDDL(t, "", "--output", "json",
		"--dsn", "postgres://nope:1/db?connect_timeout=1",
		"-e", "SELECT 1")
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)
	require.Len(t, env.Errors, 1)
	require.Equal(t, output.CodeSafetyViolation, env.Errors[0].Code)
	require.Equal(t, "SELECT", env.Errors[0].Context["tag"])
	require.Contains(t, env.Errors[0].Context["reason"], "requires a DDL statement")
}

// TestExplainDDLCmdInvalidMode mirrors TestExplainCmdInvalidMode for
// the explain-ddl surface so the --mode validation error is consistent.
func TestExplainDDLCmdInvalidMode(t *testing.T) {
	t.Setenv("CRDB_DSN", "postgres://envhost:26257/defaultdb")

	stdout, err := runExplainDDL(t, "", "--output", "json",
		"--mode", "yolo",
		"-e", "ALTER TABLE t ADD COLUMN x INT")
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.Contains(t, env.Errors[0].Message, "invalid safety mode")
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
