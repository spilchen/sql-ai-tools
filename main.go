// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Command crdb-sql is the CLI entry point for the agent-friendly
// CockroachDB SQL tooling described in the project README. It is a thin
// shell over the cobra command tree in package cmd; all behavior lives
// there.
package main

import (
	"fmt"
	"os"

	"github.com/spilchen/sql-ai-tools/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		// cmd.Execute() suppresses cobra's own error printing
		// (SilenceErrors on rootCmd) so that this is the single
		// place errors reach the user.
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
