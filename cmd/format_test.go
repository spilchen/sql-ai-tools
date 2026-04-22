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

// TestFormatCmdStripsShellPrompts verifies that pasted output from a
// cockroach sql REPL session is auto-stripped before parsing.
func TestFormatCmdStripsShellPrompts(t *testing.T) {
	pasted := "root@:26257/defaultdb> SELECT id,\n" +
		"                    ->   name\n" +
		"                    -> FROM users;\n"

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader(pasted))
	root.SetArgs([]string{"format"})

	require.NoError(t, root.Execute())
	require.Contains(t, stdout.String(), "SELECT id, name FROM users")
}

// TestFormatCmdColorAlways verifies --color=always emits ANSI escapes
// in text mode even when stdout is not a TTY (the test buffer never is).
func TestFormatCmdColorAlways(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"format", "-e", "select 1", "--color", "always"})

	require.NoError(t, root.Execute())
	require.Contains(t, stdout.String(), "\x1b[", "--color=always must emit ANSI escapes")
}

// TestFormatCmdColorNeverAndAuto verifies --color=never and --color=auto
// both produce uncolored output when stdout is a buffer (auto's TTY
// check returns false for non-*os.File writers).
func TestFormatCmdColorNeverAndAuto(t *testing.T) {
	tests := []struct {
		name string
		flag string
	}{
		{name: "never", flag: "never"},
		{name: "auto with non-TTY stdout", flag: "auto"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := newRootCmd()
			var stdout bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&bytes.Buffer{})
			root.SetArgs([]string{"format", "-e", "select 1", "--color", tc.flag})

			require.NoError(t, root.Execute())
			require.NotContains(t, stdout.String(), "\x1b[",
				"--color=%s must not emit ANSI escapes", tc.flag)
		})
	}
}

// TestFormatCmdColorAutoNoTTY verifies that --color=auto's TTY check
// returns false for a real *os.File pointing at a regular file (the
// non-character-device path of isTerminal). This complements the
// non-*os.File case in TestFormatCmdColorNeverAndAuto.
func TestFormatCmdColorAutoNoTTY(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "out.txt"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	root := newRootCmd()
	root.SetOut(f)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"format", "-e", "select 1", "--color", "auto"})

	require.NoError(t, root.Execute())
	require.NoError(t, f.Sync())

	got, err := os.ReadFile(f.Name())
	require.NoError(t, err)
	require.NotContains(t, string(got), "\x1b[",
		"--color=auto on a regular file must not emit ANSI escapes")
}

// TestFormatCmdColorNeverInJSON verifies that JSON output is never
// colorized regardless of --color, since the envelope is consumed by
// machines.
func TestFormatCmdColorNeverInJSON(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"format", "-e", "select 1", "--color", "always", "--output", "json"})

	require.NoError(t, root.Execute())
	require.NotContains(t, stdout.String(), "\x1b[", "JSON envelope must never contain ANSI escapes")

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	var payload struct {
		FormattedSQL string `json:"formatted_sql"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &payload))
	require.Equal(t, "SELECT 1", payload.FormattedSQL)
}

// TestFormatCmdColorInvalid verifies that an unknown --color value is
// rejected with a clear error message.
func TestFormatCmdColorInvalid(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"format", "-e", "select 1", "--color", "rainbow"})

	err := root.Execute()
	require.Error(t, err)
	require.ErrorContains(t, err, "invalid --color")
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
