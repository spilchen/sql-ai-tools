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
	"errors"
	"fmt"
	"os"

	"github.com/spilchen/sql-ai-tools/cmd"
	"github.com/spilchen/sql-ai-tools/internal/output"
)

func main() {
	if err := cmd.Execute(); err != nil {
		// cmd.Execute() suppresses cobra's own error printing
		// (SilenceErrors on rootCmd) so that this is the single
		// place errors reach the user. output.ErrRendered means
		// the failure was already surfaced as a JSON envelope on
		// stdout; suppress the stderr "Error: ..." line so agents
		// see exactly one structured response, but still exit
		// non-zero so shell callers notice.
		//
		// Edge case: when the error envelope itself fails to
		// marshal, RenderError returns the joined error (not
		// ErrRendered), so this branch prints it to stderr as a
		// fallback. At that point the JSON contract is already
		// broken, so a stderr line is the least-bad option.
		if !errors.Is(err, output.ErrRendered) {
			// Write error intentionally ignored: stderr is the last
			// resort and there is no further channel to report a
			// failure to write to it.
			fmt.Fprintln(os.Stderr, "Error:", err) //nolint:errcheck
		}
		os.Exit(1)
	}
}
