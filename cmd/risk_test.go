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
	"github.com/spilchen/sql-ai-tools/internal/risk"
)

// TestRiskCmdText exercises the risk subcommand's text output path
// end-to-end with a risky DELETE piped via stdin.
func TestRiskCmdText(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("DELETE FROM users"))
	root.SetArgs([]string{"risk"})

	require.NoError(t, root.Execute())

	got := stdout.String()
	require.Contains(t, got, "CRITICAL")
	require.Contains(t, got, "DELETE_NO_WHERE")
	require.Contains(t, got, "hint:")
}

// TestRiskCmdJSON exercises --output json end-to-end, verifying the
// envelope shape and the findings payload.
func TestRiskCmdJSON(t *testing.T) {
	root := newRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader("DELETE FROM users"))
	root.SetArgs([]string{"risk", "--output", "json"})

	require.NoError(t, root.Execute())
	require.Empty(t, stderr.String(), "JSON mode must not write to stderr")

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, output.TierZeroConfig, env.Tier)
	require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)
	require.NotEmpty(t, env.ParserVersion)
	require.Empty(t, env.Errors)

	var findings []risk.Finding
	require.NoError(t, json.Unmarshal(env.Data, &findings))
	require.Len(t, findings, 1)
	require.Equal(t, "DELETE_NO_WHERE", findings[0].ReasonCode)
	require.Equal(t, risk.SeverityCritical, findings[0].Severity)
	require.NotEmpty(t, findings[0].FixHint)
}

// TestRiskCmdExprFlag verifies the -e flag path.
func TestRiskCmdExprFlag(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"risk", "-e", "DROP TABLE users", "--output", "json"})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))

	var findings []risk.Finding
	require.NoError(t, json.Unmarshal(env.Data, &findings))
	require.Len(t, findings, 1)
	require.Equal(t, "DROP_TABLE", findings[0].ReasonCode)
}

// TestRiskCmdFileArg verifies reading SQL from a file argument.
func TestRiskCmdFileArg(t *testing.T) {
	dir := t.TempDir()
	sqlFile := filepath.Join(dir, "input.sql")
	require.NoError(t, os.WriteFile(sqlFile, []byte("SELECT * FROM t"), 0644))

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"risk", sqlFile, "--output", "json"})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))

	var findings []risk.Finding
	require.NoError(t, json.Unmarshal(env.Data, &findings))
	require.Len(t, findings, 1)
	require.Equal(t, "SELECT_STAR", findings[0].ReasonCode)
}

// TestRiskCmdNoFindings verifies that safe SQL produces an empty
// findings array in JSON mode and no text output.
func TestRiskCmdNoFindings(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		root := newRootCmd()
		var stdout bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&bytes.Buffer{})
		root.SetArgs([]string{"risk", "-e", "SELECT id FROM users WHERE id = 1", "--output", "json"})

		require.NoError(t, root.Execute())

		var env output.Envelope
		require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
		require.Empty(t, env.Errors)

		var findings []risk.Finding
		require.NoError(t, json.Unmarshal(env.Data, &findings))
		require.Empty(t, findings)
	})

	t.Run("text", func(t *testing.T) {
		root := newRootCmd()
		var stdout bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&bytes.Buffer{})
		root.SetArgs([]string{"risk", "-e", "SELECT id FROM users WHERE id = 1"})

		require.NoError(t, root.Execute())
		require.Empty(t, stdout.String())
	})
}

// TestRiskCmdParseErrorJSON verifies that invalid SQL in JSON mode
// produces an envelope with errors and nil data.
func TestRiskCmdParseErrorJSON(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("SELECTT 1"))
	root.SetArgs([]string{"risk", "--output", "json"})

	err := root.Execute()
	require.Error(t, err)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.NotEmpty(t, env.Errors)
	require.Nil(t, env.Data)
}

// TestRiskCmdEmptyInput verifies that empty stdin produces an error.
func TestRiskCmdEmptyInput(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader(""))
	root.SetArgs([]string{"risk"})

	require.Error(t, root.Execute())
}
