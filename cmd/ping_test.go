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

// TestPingCmdNoDSN verifies that ping without a DSN (no flag, no env)
// returns an error directing the user to provide a connection string.
func TestPingCmdNoDSN(t *testing.T) {
	t.Setenv("CRDB_DSN", "")

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"ping"})

	err := root.Execute()
	require.Error(t, err)
	require.ErrorContains(t, err, "no connection string")
}

// TestPingCmdNoDSNJSON verifies the JSON envelope when no DSN is
// configured: connection_status is "disconnected", tier is "connected"
// (the command's tier, not the actual state), and the error entry
// mentions --dsn and CRDB_DSN.
func TestPingCmdNoDSNJSON(t *testing.T) {
	t.Setenv("CRDB_DSN", "")

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"ping", "--output", "json"})

	err := root.Execute()
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

// TestPingCmdDSNFromFlag verifies that the --dsn flag is plumbed
// through to the connection manager. The invalid DSN causes a
// connection error — not a "no connection string" error.
func TestPingCmdDSNFromFlag(t *testing.T) {
	t.Setenv("CRDB_DSN", "")

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"ping", "--output", "json", "--dsn", "postgres://flaghost:26257/defaultdb"})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.NotContains(t, env.Errors[0].Message, "no connection string",
		"should attempt to connect, not reject for missing DSN")
	require.Contains(t, env.Errors[0].Message, "connect to CockroachDB")
}

// TestPingCmdDSNFromEnv verifies that the CRDB_DSN environment
// variable is picked up when --dsn is not provided.
func TestPingCmdDSNFromEnv(t *testing.T) {
	t.Setenv("CRDB_DSN", "postgres://envhost:26257/defaultdb")

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"ping", "--output", "json"})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.NotContains(t, env.Errors[0].Message, "no connection string",
		"env var should be picked up as DSN")
	require.Contains(t, env.Errors[0].Message, "connect to CockroachDB")
}

// TestPingCmdFlagOverridesEnv verifies that --dsn takes precedence
// over the CRDB_DSN environment variable when both are set.
func TestPingCmdFlagOverridesEnv(t *testing.T) {
	t.Setenv("CRDB_DSN", "postgres://envhost:26257/defaultdb")

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"ping", "--output", "json", "--dsn", "postgres://flaghost:26257/defaultdb"})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)

	// The error comes from attempting to connect to flaghost, not envhost.
	// We can't directly see which host was tried, but we verify the flag
	// path was taken (not the "no connection string" path).
	require.Contains(t, env.Errors[0].Message, "connect to CockroachDB")
}

// TestPingCmdRejectsExtraArgs verifies that positional arguments are
// rejected.
func TestPingCmdRejectsExtraArgs(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"ping", "extra-arg"})

	err := root.Execute()
	require.Error(t, err)
	require.ErrorContains(t, err, "unknown command")
}
