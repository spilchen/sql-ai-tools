// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package versionroute selects the per-quarter crdb-sql backend that
// matches the user's --target-version and re-execs into it. CockroachDB
// releases follow Year.Quarter.Patch (e.g. 25.4.0), and parser
// behavior can change between quarters; this package is the
// router that turns "the user asked for 25.4" into "exec
// crdb-sql-v254".
//
// Layout: a single crdb-sql distribution ships one latest binary
// (crdb-sql) plus optional per-quarter siblings (crdb-sql-v251,
// crdb-sql-v262, ...). MaybeReexec, called from main as the very
// first action, scans os.Args for --target-version, computes the
// requested Quarter, compares it to the binary's compiled Quarter,
// and execs the matching sibling on a mismatch. Missing sibling is a
// hard error — silent fallback to the wrong parser is the bug this
// package exists to prevent.
package versionroute

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
)

// Quarter identifies a CockroachDB quarterly release. Year is the
// year suffix (e.g. 25 for 2025) and Q is the quarter ordinal in 1..4.
// The zero value is the sentinel for "no quarter known" (e.g. an
// unstamped local build whose parser dep is missing from BuildInfo).
//
// Construct non-zero values via MakeQuarter, ParseFromTarget, ParseTag,
// or parseForkVersion — those are the only producers that enforce the
// year>=1 / 1<=Q<=4 invariant. The fields are exported for ergonomic
// reads (comparison, map keys) but a caller writing a literal struct
// is responsible for validity; the formatter methods (String, Tag,
// BackendName) handle the zero value cleanly but make no other claims
// for out-of-range fields.
type Quarter struct {
	Year int
	Q    int
}

// MakeQuarter constructs a Quarter and validates that year>=1 and
// 1<=q<=4. Returns the zero Quarter and a non-nil error on bad input.
// Use this wherever a Quarter is built from numeric inputs that have
// not already been validated by one of the parsers; the parsers
// themselves route through here so all Quarter construction sites
// share one enforcement point.
func MakeQuarter(year, q int) (Quarter, error) {
	if year < 1 {
		return Quarter{}, fmt.Errorf("quarter year must be >= 1, got %d", year)
	}
	if q < 1 || q > 4 {
		return Quarter{}, fmt.Errorf("quarter ordinal must be in 1..4, got %d", q)
	}
	return Quarter{Year: year, Q: q}, nil
}

// IsZero reports whether q is the zero value, used as a sentinel for
// "no quarter known" (e.g. when the binary was built without a stamp
// and BuildInfo lacks the parser dep).
func (q Quarter) IsZero() bool { return q.Year == 0 && q.Q == 0 }

// String returns the human-readable form (e.g. "25.4"), or "unknown"
// for the zero value. Used in error messages and discovery output;
// callers do not need to gate on IsZero before invoking it.
func (q Quarter) String() string {
	if q.IsZero() {
		return "unknown"
	}
	return strconv.Itoa(q.Year) + "." + strconv.Itoa(q.Q)
}

// Tag returns the binary-suffix form used in filenames and ldflag
// stamps (e.g. "v254"), or "" for the zero value. This is the
// canonical short identifier that appears in crdb-sql-v254,
// build/go.v254.mod, and the builtQuarterStamp ldflag.
func (q Quarter) Tag() string {
	if q.IsZero() {
		return ""
	}
	return "v" + strconv.Itoa(q.Year) + strconv.Itoa(q.Q)
}

// BackendName returns the on-disk backend filename for this quarter
// (e.g. "crdb-sql-v254"). For the zero value returns "crdb-sql"
// (the unsuffixed latest binary), so callers can use this method
// uniformly when rendering discovery rows. Do not append a platform
// suffix here; callers add ".exe" on Windows via execSuffix.
func (q Quarter) BackendName() string {
	if q.IsZero() {
		return "crdb-sql"
	}
	return "crdb-sql-" + q.Tag()
}

// ParseFromTarget extracts the Quarter from a --target-version value.
// Accepted shapes mirror output.ValidateTargetVersion: MAJOR.MINOR or
// MAJOR.MINOR.PATCH, optionally prefixed with a single 'v'. Only the
// MAJOR (Year) and MINOR (Quarter) components affect routing — patch
// is ignored.
//
// Returns (zero, false) on any parse failure. MaybeReexec must remain
// permissive on bad input: this code runs before cobra's flag parser,
// and a malformed --target-version will surface a structured error
// from cobra later. We must not panic or print here.
func ParseFromTarget(s string) (Quarter, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return Quarter{}, false
	}
	year, err := strconv.Atoi(parts[0])
	if err != nil {
		return Quarter{}, false
	}
	q, err := strconv.Atoi(parts[1])
	if err != nil {
		return Quarter{}, false
	}
	out, err := MakeQuarter(year, q)
	if err != nil {
		return Quarter{}, false
	}
	return out, true
}

// ParseTag is the inverse of Quarter.Tag. It accepts a binary-suffix
// form ("v254" or "254") and returns the parsed Quarter. Used by
// discovery when scanning filenames in $PATH or the install dir.
//
// Returns (zero, false) for any string that is not exactly an
// optional 'v' followed by a Year.Quarter pair where each component
// fits the constraints in ParseFromTarget. The expected layout is
// 1-2 digits of Year followed by exactly one quarter digit (1-4),
// because real CRDB versions never use Q=0 or Q>4.
func ParseTag(s string) (Quarter, bool) {
	s = strings.TrimPrefix(s, "v")
	if len(s) < 2 {
		return Quarter{}, false
	}
	// The quarter digit is always the last character (1-4), and the
	// remaining prefix is the year. This split is unambiguous because
	// CRDB quarters are single-digit (the parsers reject q > 4).
	qStr := s[len(s)-1:]
	yearStr := s[:len(s)-1]
	year, err := strconv.Atoi(yearStr)
	if err != nil {
		return Quarter{}, false
	}
	q, err := strconv.Atoi(qStr)
	if err != nil {
		return Quarter{}, false
	}
	out, err := MakeQuarter(year, q)
	if err != nil {
		return Quarter{}, false
	}
	return out, true
}

// builtQuarterStamp is set at build time via -ldflags
// "-X github.com/spilchen/sql-ai-tools/internal/versionroute.builtQuarterStamp=v262"
// by the per-quarter make targets (build-v262, build-latest, ...).
// Empty in unstamped local builds (`go build`, `go test`, `go run`),
// in which case Built falls back to BuildInfo-derived inference.
var builtQuarterStamp = ""

// parserModulePath duplicates the constant in cmd/version.go to avoid
// a cmd<-internal/versionroute import cycle (cmd depends on this
// package via main, not the other way around).
const parserModulePath = "github.com/cockroachdb/cockroachdb-parser"

// Built returns the Quarter this binary represents. Resolution order:
//
//  1. Filename. A binary installed as `crdb-sql-v254[.exe]` declares
//     its quarter via the suffix, and the filename is authoritative
//     over any compiled-in stamp. This makes routing self-consistent
//     under copies/renames (e.g. a hand-installed sibling) and
//     prevents exec loops where a binary stamped v262 sits on disk
//     as crdb-sql-v251 and tries to exec "itself" forever.
//  2. Build-time ldflag stamp (builtQuarterStamp), set by the
//     `make build` / `make build-vXXX` targets. This covers the
//     unsuffixed latest binary (`crdb-sql`), whose filename carries
//     no quarter info.
//  3. BuildInfo fallback for unstamped local builds (`go run`,
//     `go build` results without ldflag stamping). `go test`
//     binaries typically have no parser dep recorded in BuildInfo,
//     so this branch returns (zero, false) under tests — which is
//     the correct outcome (tests don't route).
//
// Returns (zero, false) when no source yields a usable Quarter.
// Callers that need to know *why* the answer is zero (specifically
// to distinguish "no info" from "stamp is malformed and should be
// fixed") should consult StampDiagnostic afterward.
func Built() (Quarter, bool) {
	if q, ok := quarterFromExecutable(); ok {
		return q, true
	}
	if builtQuarterStamp != "" {
		if q, ok := ParseTag(builtQuarterStamp); ok {
			return q, true
		}
		// Malformed stamp surfaces via StampDiagnostic; the routing
		// path treats this as "quarter unknown" and the diagnostic
		// makes the underlying cause visible to the operator.
		return Quarter{}, false
	}
	return builtQuarterFromBuildInfo()
}

// StampDiagnostic returns a non-empty string when builtQuarterStamp
// was set at build time but does not parse as a Quarter tag. The
// returned string is suitable for one-line stderr output. Returns ""
// when the stamp is absent or valid (the common cases).
//
// MaybeReexec emits this diagnostic before any routing decision so
// operators see the build-pipeline bug directly rather than through
// the second-order symptom of a confusing missing-backend error.
func StampDiagnostic() string {
	if builtQuarterStamp == "" {
		return ""
	}
	if _, ok := ParseTag(builtQuarterStamp); ok {
		return ""
	}
	return fmt.Sprintf(
		"crdb-sql: build stamp %q is not a valid quarter tag "+
			"(expected v<year><quarter> like v262 = CRDB 26.2); "+
			"routing this invocation as if no stamp were set. Reinstall to fix.",
		builtQuarterStamp,
	)
}

// quarterFromExecutable extracts the Quarter from the running
// binary's filename if it follows the crdb-sql-vXXX convention.
// Returns (zero, false) for the unsuffixed `crdb-sql` (which uses
// the stamp instead) or any other name.
func quarterFromExecutable() (Quarter, bool) {
	exe, err := os.Executable()
	if err != nil {
		return Quarter{}, false
	}
	base := filepath.Base(exe)
	// Strip the platform-specific executable suffix; centralized in
	// discover.go, but we duplicate the check here as a constant
	// rather than importing it to keep this function dependency-light
	// (Built may be called very early, before any other state exists).
	base = strings.TrimSuffix(base, ".exe")
	const prefix = "crdb-sql-"
	if !strings.HasPrefix(base, prefix) {
		return Quarter{}, false
	}
	return ParseTag(strings.TrimPrefix(base, prefix))
}

// builtQuarterFromBuildInfo derives the Quarter from the parser
// module's resolved version in debug.BuildInfo.Deps. Mirrors the
// resolution logic in cmd.parserVersionFrom but extracts the
// Year.Quarter rather than returning the raw module-version string.
//
// The parser fork uses tags of the form v0.YEAR.QUARTER (e.g.
// v0.26.2 for CRDB 26.2). The leading "0." prefix is the fork's
// "API version" placeholder; the CRDB version lives at indices 1-2.
func builtQuarterFromBuildInfo() (Quarter, bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return Quarter{}, false
	}
	for _, dep := range info.Deps {
		if dep.Path != parserModulePath {
			continue
		}
		ver := ""
		if dep.Replace != nil && dep.Replace.Version != "" {
			ver = dep.Replace.Version
		} else if dep.Version != "" {
			ver = dep.Version
		}
		if ver == "" {
			return Quarter{}, false
		}
		return parseForkVersion(ver)
	}
	return Quarter{}, false
}

// parseForkVersion turns the parser fork's module version
// (e.g. "v0.26.2") into a Quarter. Splits on '.', drops the leading
// "v0" (API-version placeholder), and treats the next two components
// as Year and Quarter. Returns false on any other shape.
func parseForkVersion(ver string) (Quarter, bool) {
	ver = strings.TrimPrefix(ver, "v")
	parts := strings.Split(ver, ".")
	if len(parts) < 3 {
		return Quarter{}, false
	}
	// parts[0] is the fork's API-version digit; the CRDB Year.Quarter
	// is at indices 1 and 2. parts[1] may be 2 digits (year),
	// parts[2] is the quarter (single digit) optionally followed by a
	// patch suffix joined with '-' or further '.'.
	year, err := strconv.Atoi(parts[1])
	if err != nil {
		return Quarter{}, false
	}
	// parts[2] may be "2" or "2-rc1"; take the leading digits.
	qStr := parts[2]
	if idx := strings.IndexAny(qStr, "-+"); idx >= 0 {
		qStr = qStr[:idx]
	}
	q, err := strconv.Atoi(qStr)
	if err != nil {
		return Quarter{}, false
	}
	out, err := MakeQuarter(year, q)
	if err != nil {
		return Quarter{}, false
	}
	return out, true
}

// extractTargetVersion scans args for a --target-version value
// (either "--target-version 25.4.0" or "--target-version=25.4.0").
// Returns the raw value and a bool. Empty value with a present flag
// returns ("", true) so callers can distinguish "absent" from "empty".
//
// We do our own scan rather than delegate to cobra because cobra
// hasn't been initialized yet — MaybeReexec runs before main wires up
// the command tree. The parser is intentionally minimal: it does not
// understand subcommand boundaries (the flag is persistent on the
// root, so it can appear anywhere in argv) but it does honor the
// POSIX "--" end-of-options marker — anything past "--" is a
// positional token (e.g. inline SQL) and must not be treated as the
// flag, even if it textually contains "--target-version=".
//
// args[0] is the program name and is skipped; nobody invokes a
// binary with --target-version as argv[0].
func extractTargetVersion(args []string) (string, bool) {
	const flag = "--" + targetVersionFlagName
	const flagEq = flag + "="
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return "", false
		}
		switch {
		case a == flag:
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		case strings.HasPrefix(a, flagEq):
			return strings.TrimPrefix(a, flagEq), true
		}
	}
	return "", false
}

// targetVersionFlagName mirrors cmd.targetVersionFlag. Duplicated to
// avoid an import cycle and because this scan must work without
// importing cobra.
const targetVersionFlagName = "target-version"
