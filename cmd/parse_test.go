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
	"github.com/spilchen/sql-ai-tools/internal/sqlparse"
)

// TestParseCmdText exercises the parse subcommand's text output path
// end-to-end. The input is piped via stdin; the output is tab-separated
// TYPE\tTAG\tSQL per line.
func TestParseCmdText(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("SELECT 1"))
	root.SetArgs([]string{"parse"})

	require.NoError(t, root.Execute())

	got := stdout.String()
	require.Contains(t, got, "DML\tSELECT\tSELECT 1")
}

// TestParseCmdJSON exercises --output json end-to-end, verifying the
// envelope shape and the data payload for a multi-statement input.
func TestParseCmdJSON(t *testing.T) {
	root := newRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader("SELECT 1; CREATE TABLE t (a INT)"))
	root.SetArgs([]string{"parse", "--output", "json"})

	require.NoError(t, root.Execute())
	require.Empty(t, stderr.String(), "JSON mode must not write to stderr")

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, output.TierZeroConfig, env.Tier)
	require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)
	require.NotEmpty(t, env.ParserVersion)
	require.Empty(t, env.Errors)

	var stmts []sqlparse.ClassifiedStatement
	require.NoError(t, json.Unmarshal(env.Data, &stmts))
	require.Len(t, stmts, 2)

	require.Equal(t, sqlparse.StatementTypeDML, stmts[0].StatementType)
	require.Equal(t, "SELECT", stmts[0].Tag)

	require.Equal(t, sqlparse.StatementTypeDDL, stmts[1].StatementType)
	require.Equal(t, "CREATE TABLE", stmts[1].Tag)
}

// TestParseCmdExprFlag verifies the -e flag path.
func TestParseCmdExprFlag(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"parse", "-e", "BEGIN", "--output", "json"})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))

	var stmts []sqlparse.ClassifiedStatement
	require.NoError(t, json.Unmarshal(env.Data, &stmts))
	require.Len(t, stmts, 1)
	require.Equal(t, sqlparse.StatementTypeTCL, stmts[0].StatementType)
}

// TestParseCmdFileArg verifies reading SQL from a file argument.
func TestParseCmdFileArg(t *testing.T) {
	dir := t.TempDir()
	sqlFile := filepath.Join(dir, "input.sql")
	require.NoError(t, os.WriteFile(sqlFile, []byte("DROP TABLE t"), 0644))

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"parse", sqlFile, "--output", "json"})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))

	var stmts []sqlparse.ClassifiedStatement
	require.NoError(t, json.Unmarshal(env.Data, &stmts))
	require.Len(t, stmts, 1)
	require.Equal(t, sqlparse.StatementTypeDDL, stmts[0].StatementType)
	require.Equal(t, "DROP TABLE", stmts[0].Tag)
}

// TestParseCmdParseErrorText verifies that invalid SQL in text mode
// surfaces as a non-nil error from Execute.
func TestParseCmdParseErrorText(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("SELECTT 1"))
	root.SetArgs([]string{"parse"})

	require.Error(t, root.Execute())
}

// TestParseCmdParseErrorJSON verifies that invalid SQL in JSON mode
// produces an envelope with errors and nil data.
func TestParseCmdParseErrorJSON(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("SELECTT 1"))
	root.SetArgs([]string{"parse", "--output", "json"})

	err := root.Execute()
	require.Error(t, err)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.NotEmpty(t, env.Errors)
	require.Nil(t, env.Data)
}

// TestParseCmdEmptyInput verifies that empty stdin produces an error.
func TestParseCmdEmptyInput(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader(""))
	root.SetArgs([]string{"parse"})

	require.Error(t, root.Execute())
}

// TestParseCmdConflictingInput verifies that -e and a file arg together
// produce an error.
func TestParseCmdConflictingInput(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"parse", "-e", "SELECT 1", "somefile.sql"})

	err := root.Execute()
	require.Error(t, err)
	require.ErrorContains(t, err, "cannot use -e flag and file argument together")
}
