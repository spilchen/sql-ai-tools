// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMCPCmdRegistered guards the cobra wiring: the `mcp` subcommand
// must be discoverable from a fresh root, and `mcp --help` must succeed
// and print the subcommand's Use line. We deliberately do not exercise
// the stdio loop here — that requires speaking JSON-RPC and belongs to
// the manual "register with Claude Code" verification in the README.
func TestMCPCmdRegistered(t *testing.T) {
	root := newRootCmd()

	sub, _, err := root.Find([]string{"mcp"})
	require.NoError(t, err)
	require.NotNil(t, sub)
	require.Equal(t, "mcp", sub.Name())

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"mcp", "--help"})
	require.NoError(t, root.Execute())
	require.Contains(t, buf.String(), "crdb-sql mcp")
}

// TestMCPCmdRejectsExtraArgs locks in cobra.NoArgs on the mcp
// subcommand so an accidental switch (e.g. to ArbitraryArgs) — which
// would let a stray argument silently swallow stdio — fails loudly.
func TestMCPCmdRejectsExtraArgs(t *testing.T) {
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"mcp", "oops"})

	require.Error(t, root.Execute())
}
