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

// TestFormatCmdText exercises the format subcommand's text output path
// end-to-end. The input is piped via stdin; the output is the
// pretty-printed SQL followed by a newline.
func TestFormatCmdText(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("select  id,name  from  users"))
	root.SetArgs([]string{"format"})

	require.NoError(t, root.Execute())

	got := stdout.String()
	require.Contains(t, got, "SELECT id, name FROM users")
}

// TestFormatCmdJSON exercises --output json end-to-end, verifying the
// envelope shape and the data payload.
func TestFormatCmdJSON(t *testing.T) {
	root := newRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader("select 1"))
	root.SetArgs([]string{"format", "--output", "json"})

	require.NoError(t, root.Execute())
	require.Empty(t, stderr.String(), "JSON mode must not write to stderr")

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, output.TierZeroConfig, env.Tier)
	require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)
	require.NotEmpty(t, env.ParserVersion)
	require.Empty(t, env.Errors)

	var payload struct {
		FormattedSQL string `json:"formatted_sql"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &payload))
	require.Equal(t, "SELECT 1", payload.FormattedSQL)
}

// TestFormatCmdExprFlag verifies the -e flag path.
func TestFormatCmdExprFlag(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"format", "-e", "select  1"})

	require.NoError(t, root.Execute())
	require.Contains(t, stdout.String(), "SELECT 1")
}

// TestFormatCmdFileArg verifies reading SQL from a file argument.
func TestFormatCmdFileArg(t *testing.T) {
	dir := t.TempDir()
	sqlFile := filepath.Join(dir, "input.sql")
	require.NoError(t, os.WriteFile(sqlFile, []byte("select  1"), 0644))

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"format", sqlFile})

	require.NoError(t, root.Execute())
	require.Contains(t, stdout.String(), "SELECT 1")
}

// TestFormatCmdMultiStatement verifies that multi-statement input is
// formatted with semicolon-newline separators in both text and JSON.
func TestFormatCmdMultiStatement(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		root := newRootCmd()
		var stdout bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&bytes.Buffer{})
		root.SetIn(strings.NewReader("select 1; select 2"))
		root.SetArgs([]string{"format"})

		require.NoError(t, root.Execute())
		require.Equal(t, "SELECT 1;\nSELECT 2\n", stdout.String())
	})

	t.Run("json", func(t *testing.T) {
		root := newRootCmd()
		var stdout bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&bytes.Buffer{})
		root.SetIn(strings.NewReader("select 1; select 2"))
		root.SetArgs([]string{"format", "--output", "json"})

		require.NoError(t, root.Execute())

		var env output.Envelope
		require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))

		var payload struct {
			FormattedSQL string `json:"formatted_sql"`
		}
		require.NoError(t, json.Unmarshal(env.Data, &payload))
		require.Equal(t, "SELECT 1;\nSELECT 2", payload.FormattedSQL)
	})
}

// TestFormatCmdParseErrorText verifies that invalid SQL in text mode
// renders an enriched diagnostic with position and SQLSTATE code.
func TestFormatCmdParseErrorText(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("SELECT FROM"))
	root.SetArgs([]string{"format"})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	got := stdout.String()
	require.Contains(t, got, "1:12:")
	require.Contains(t, got, "syntax error")
	require.Contains(t, got, "42601")
}

// TestFormatCmdParseErrorJSON verifies that invalid SQL in JSON mode
// produces an envelope with a structured error containing SQLSTATE
// code, severity, category, and source position.
func TestFormatCmdParseErrorJSON(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("SELECT FROM"))
	root.SetArgs([]string{"format", "--output", "json"})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.Nil(t, env.Data)

	diagErr := env.Errors[0]
	require.Equal(t, "42601", diagErr.Code)
	require.Equal(t, output.SeverityError, diagErr.Severity)
	require.Equal(t, "syntax_error", diagErr.Category)
	require.Contains(t, diagErr.Message, "syntax error")
	require.NotNil(t, diagErr.Position)
	require.Equal(t, 1, diagErr.Position.Line)
	require.Equal(t, 12, diagErr.Position.Column)
	require.Equal(t, 11, diagErr.Position.ByteOffset)
}

// TestFormatCmdEmptyInput verifies that empty stdin produces an error.
func TestFormatCmdEmptyInput(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader(""))
	root.SetArgs([]string{"format"})

	require.Error(t, root.Execute())
}

// TestFormatCmdConflictingInput verifies that -e and a file arg together
// produce an error.
func TestFormatCmdConflictingInput(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"format", "-e", "SELECT 1", "somefile.sql"})

	err := root.Execute()
	require.Error(t, err)
	require.ErrorContains(t, err, "cannot use -e flag and file argument together")
}
