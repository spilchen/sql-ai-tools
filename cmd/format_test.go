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

// TestFormatCmdParseErrorText verifies that invalid SQL in text mode
// surfaces as a non-nil error from Execute.
func TestFormatCmdParseErrorText(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("SELECTT 1"))
	root.SetArgs([]string{"format"})

	require.Error(t, root.Execute())
}

// TestFormatCmdParseErrorJSON verifies that invalid SQL in JSON mode
// produces an envelope with errors and nil data.
func TestFormatCmdParseErrorJSON(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("SELECTT 1"))
	root.SetArgs([]string{"format", "--output", "json"})

	err := root.Execute()
	require.Error(t, err)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.NotEmpty(t, env.Errors)
	require.Nil(t, env.Data)
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
