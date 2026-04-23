// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package versionroute

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// MaybeReexec inspects os.Args for a --target-version, and if it
// requests a different Year.Quarter than the running binary, locates
// the matching sibling backend and re-execs into it. This must be the
// first call in main() — before flag parsing, builtin registration,
// or any work that depends on the parser version. Routing earlier
// guarantees the eventual cobra/parsing pipeline runs in the correct
// per-quarter binary.
//
// On a successful re-exec the call does not return (the process is
// replaced on Unix, or this process exits with the child's status on
// Windows). On a routing-not-needed outcome (no flag, malformed flag,
// matching quarter, or unstamped binary) the function returns and
// main() proceeds normally.
//
// On a routing-needed-but-impossible outcome (sibling missing) the
// function writes a discovery-hint message to stderr and exits with
// status 2. Silent fallback to the wrong parser would defeat the
// reason this package exists.
func MaybeReexec() {
	maybeReexec(os.Args, os.Stderr, osExitFunc)
}

// osExitFunc indirects os.Exit so tests can substitute a panic and
// recover the exit code without terminating the test binary. Production
// callers always use the package-level os.Exit.
var osExitFunc = os.Exit

// maybeReexec is the testable core of MaybeReexec. Splitting them out
// lets tests inject args, capture stderr, and observe the requested
// exit code without calling os.Exit on the test process.
func maybeReexec(args []string, stderr io.Writer, exit func(int)) {
	// Capture the stamp diagnostic once. Surface it up front so
	// operators see build-pipeline bugs directly rather than as a
	// second-order symptom of a confusing routing failure later, and
	// reuse the captured string in the missing-backend hint below so
	// both messages agree even if global stamp state ever changes
	// between calls.
	stampDiag := StampDiagnostic()
	if stampDiag != "" {
		writeAll(stderr, stampDiag+"\n")
	}
	rawTarget, hasFlag := extractTargetVersion(args)
	if !hasFlag {
		return
	}
	want, ok := ParseFromTarget(rawTarget)
	if !ok {
		// Malformed --target-version. Cobra's flag parser will reject
		// it later with a structured error; routing must not pre-empt
		// that diagnostic. Returning here is the only correct choice.
		return
	}
	built, builtKnown := Built()
	if builtKnown && want.Year == built.Year && want.Q == built.Q {
		return
	}
	// At this point we know we need a different backend. If our own
	// quarter is unknown (unstamped dev build with no parser dep),
	// still attempt the route — the user's intent is unambiguous.
	path, found := FindBackend(want)
	if !found {
		writeMissingBackendError(stderr, want, built, builtKnown, rawTarget, stampDiag)
		exit(2)
		return
	}
	if err := execBackend(path, args); err != nil {
		writeAll(stderr, fmt.Sprintf("crdb-sql: failed to exec %s backend at %s: %v\n",
			want.Tag(), path, err))
		exit(2)
	}
}

// writeMissingBackendError formats the discovery-hint message
// described in the plan: which backend is needed, what this binary
// is, and what alternative backends are reachable. The list helps
// users decide between installing the missing backend or omitting
// --target-version.
//
// stampDiag is the value StampDiagnostic returned at the top of
// maybeReexec; passing it in (rather than re-evaluating here)
// guarantees the diagnostic and the missing-backend hint reflect the
// same stamp state even across hypothetical future global mutations.
//
// Builds the message in memory and emits it as a single Write so
// individual line failures do not partially print and so the caller
// only has to ignore one Write error (stderr is already the last
// resort; cascading failures up further has no useful destination).
func writeMissingBackendError(
	w io.Writer, want, built Quarter, builtKnown bool, rawTarget, stampDiag string,
) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "crdb-sql: --target-version %s requires the %s backend,\n",
		rawTarget, want.BackendName())
	sb.WriteString("which is not installed alongside this binary or in $PATH.\n")
	switch {
	case builtKnown:
		fmt.Fprintf(&sb, "This binary is %s.\n", built.BackendName())
	case stampDiag != "":
		// Malformed stamp is the root cause; stampDiag was already
		// printed at MaybeReexec entry. Repeat the bad value here so
		// the missing-backend message is self-contained for log
		// scrapers that only see this block.
		fmt.Fprintf(&sb, "This binary's quarter is unknown (build stamp %q is malformed; reinstall to fix).\n",
			builtQuarterStamp)
	default:
		sb.WriteString("This binary's quarter is unknown (no build stamp, no parser dep in BuildInfo).\n")
	}
	backends := Discover()
	if len(backends) == 0 {
		sb.WriteString("No backends discovered.\n")
	} else {
		sb.WriteString("Available backends:\n")
		for _, b := range backends {
			label := b.Path
			if b.IsSelf {
				label = "(this binary)"
			}
			name := b.Quarter.BackendName()
			if b.Quarter.IsZero() {
				name = "crdb-sql (unknown quarter)"
			}
			fmt.Fprintf(&sb, "  %-18s %s\n", name, label)
		}
	}
	fmt.Fprintf(&sb, "Install the %s backend from the GitHub release, or omit --target-version\n", want.Tag())
	sb.WriteString("to use the latest parser.\n")
	writeAll(w, sb.String())
}

// writeAll emits s to w and intentionally drops the Write error.
// Stderr is the last reporting channel available at this layer; if it
// fails, there is nowhere else to send a diagnostic. The exit-code
// path still runs, so shell callers and CI still observe the failure.
func writeAll(w io.Writer, s string) {
	_, _ = w.Write([]byte(s))
}
