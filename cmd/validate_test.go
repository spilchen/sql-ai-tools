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

// TestValidateCmdTextSuccess verifies that valid SQL in text mode
// prints "Valid." to stdout.
func TestValidateCmdTextSuccess(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("SELECT 1"))
	root.SetArgs([]string{"validate"})

	require.NoError(t, root.Execute())
	require.Equal(t, "Valid.\n", stdout.String())
}

// TestValidateCmdJSONSuccess verifies that valid SQL in JSON mode
// produces an envelope with {"valid": true} data and no errors.
func TestValidateCmdJSONSuccess(t *testing.T) {
	root := newRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader("SELECT 1"))
	root.SetArgs([]string{"validate", "--output", "json"})

	require.NoError(t, root.Execute())
	require.Empty(t, stderr.String(), "JSON mode must not write to stderr")

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, output.TierZeroConfig, env.Tier)
	require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)
	require.NotEmpty(t, env.ParserVersion)
	require.Empty(t, env.Errors)

	var payload struct {
		Valid bool `json:"valid"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &payload))
	require.True(t, payload.Valid)
}

// TestValidateCmdTextError verifies that invalid SQL in text mode
// outputs a formatted error line with position and SQLSTATE code.
func TestValidateCmdTextError(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("SELECT FROM"))
	root.SetArgs([]string{"validate"})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	got := stdout.String()
	require.Contains(t, got, "1:12:")
	require.Contains(t, got, "syntax error")
	require.Contains(t, got, "42601")
}

// TestValidateCmdJSONError verifies that invalid SQL in JSON mode
// produces an envelope with structured errors and nil data.
func TestValidateCmdJSONError(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("SELECT FROM"))
	root.SetArgs([]string{"validate", "--output", "json"})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.Nil(t, env.Data)

	diagErr := env.Errors[0]
	require.Equal(t, "42601", diagErr.Code)
	require.Equal(t, output.SeverityError, diagErr.Severity)
	require.Contains(t, diagErr.Message, "syntax error")
	require.NotNil(t, diagErr.Position)
	require.Equal(t, 1, diagErr.Position.Line)
	require.Equal(t, 12, diagErr.Position.Column)
	require.Equal(t, 11, diagErr.Position.ByteOffset)
}

// TestValidateCmdExprFlag verifies the -e flag path.
func TestValidateCmdExprFlag(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"validate", "-e", "SELECT 1"})

	require.NoError(t, root.Execute())
	require.Equal(t, "Valid.\n", stdout.String())
}

// TestValidateCmdFileArg verifies reading SQL from a file argument.
func TestValidateCmdFileArg(t *testing.T) {
	dir := t.TempDir()
	sqlFile := filepath.Join(dir, "input.sql")
	require.NoError(t, os.WriteFile(sqlFile, []byte("SELECT 1"), 0644))

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"validate", sqlFile})

	require.NoError(t, root.Execute())
	require.Equal(t, "Valid.\n", stdout.String())
}

// TestValidateCmdEmptyInput verifies that empty stdin produces an error.
func TestValidateCmdEmptyInput(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader(""))
	root.SetArgs([]string{"validate"})

	require.Error(t, root.Execute())
}

// TestValidateCmdConflictingInput verifies that -e and a file arg
// together produce an error.
func TestValidateCmdConflictingInput(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"validate", "-e", "SELECT 1", "somefile.sql"})

	err := root.Execute()
	require.Error(t, err)
	require.ErrorContains(t, err, "cannot use -e flag and file argument together")
}

// TestValidateCmdMultiStatementSuccess verifies that multiple valid
// statements are all accepted.
func TestValidateCmdMultiStatementSuccess(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("SELECT 1; SELECT 2"))
	root.SetArgs([]string{"validate"})

	require.NoError(t, root.Execute())
	require.Equal(t, "Valid.\n", stdout.String())
}

// TestValidateCmdMultiStatementError verifies that an error in a later
// statement reports the correct position relative to the full input.
func TestValidateCmdMultiStatementError(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("SELECT 1;\nSELECT FROM"))
	root.SetArgs([]string{"validate", "--output", "json"})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)

	pos := env.Errors[0].Position
	require.NotNil(t, pos)
	require.Equal(t, 2, pos.Line)
	require.Equal(t, 12, pos.Column)
	require.Equal(t, 21, pos.ByteOffset)
}
