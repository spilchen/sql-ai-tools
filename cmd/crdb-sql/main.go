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
	"github.com/spilchen/sql-ai-tools/internal/builtinstubs"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/versionroute"
)

func main() {
	// Routing must happen before any work that depends on the parser
	// version (builtin registration, cobra wiring). When --target-version
	// requests a different Year.Quarter than this binary was built
	// against, MaybeReexec replaces the process with the matching
	// crdb-sql-vXXX sibling and never returns.
	versionroute.MaybeReexec()
	// Pick the stubs version that matches this binary's compiled
	// parser. With multiple registered versions Init("") always
	// picks DefaultVersion, which is wrong for sibling binaries
	// built against an older parser.
	//
	// Built() consults the executable filename, then the ldflag
	// stamp, then BuildInfo (see versionroute.Built). When all three
	// fail (e.g. a hand-rolled `go build` with no stamp and no
	// parser dep recorded in BuildInfo) it returns ok=false and we
	// fall back to DefaultVersion. Consequence: the wrong builtin
	// set is registered, so semantic checks may accept v26.2-only
	// function calls on a v26.1 parser (or vice-versa) and the
	// version envelope misreports builtins-stubs. Release binaries
	// always carry the stamp, so this only fires on hand-built dev
	// binaries; that's why we don't emit a stderr warning here (it
	// would add noise to every invocation). Malformed-stamp cases
	// already get a loud diagnostic from versionroute.StampDiagnostic.
	stubsVersion := ""
	if q, ok := versionroute.Built(); ok {
		stubsVersion = "v" + q.String()
	}
	builtinstubs.Init(stubsVersion)
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
