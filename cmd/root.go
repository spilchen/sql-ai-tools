// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package cmd hosts the cobra command tree for the crdb-sql CLI.
//
// The root is a thin shell: each subcommand (validate, format, parse,
// etc.) is defined in its own file and attached via newRootCmd, which
// builds a fresh tree per call. Avoiding package-global commands keeps
// tests independent (no flag-state leakage between t.Run cases) and
// removes the need for init()-time registration.
package cmd

import (
	"context"

	"github.com/spf13/cobra"
)

// newRootCmd builds a fresh root command with all subcommands attached.
// Construct one per Execute call (and per test) so cobra's parsed-flag
// state never leaks across invocations.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "crdb-sql",
		Short: "Agent-friendly SQL tooling for CockroachDB",
		Long: `crdb-sql exposes CockroachDB's parser, type system, and structured
error infrastructure as a CLI (and, eventually, an MCP server) so that
AI agents can validate, format, and reason about CockroachDB SQL without
round-tripping through a live cluster.`,
		// Both silences are deliberate: cobra should neither print the
		// usage dump on a runtime error (noisy) nor print the error
		// itself (we want a single source of truth). The Execute caller
		// owns error printing and exit-code translation; do not flip
		// these without updating that caller.
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newVersionCmd())
	root.AddCommand(newMCPCmd())
	return root
}

// Execute runs the root command against process arguments and returns
// whatever cobra surfaces. It does not print the error or call
// os.Exit; the caller owns that translation. This keeps the cmd
// package importable from tests without side effects on process state.
func Execute() error {
	return newRootCmd().ExecuteContext(context.Background())
}
