// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package output

import (
	"fmt"
	"strconv"
	"strings"
)

// ValidateTargetVersion parses a user-supplied CockroachDB target
// version string and returns its canonical form. The accepted shapes
// are MAJOR.MINOR and MAJOR.MINOR.PATCH, optionally prefixed with a
// single "v" (so "v25.4.0" and "25.4.0" are both accepted). The
// canonical form differs from the input in exactly one way: a leading
// "v" is stripped. Patch presence/absence and exact digits are
// preserved verbatim, so "25.4" and "25.4.0" remain distinct strings
// even though they represent the same minor release.
//
// Surrounding whitespace is trimmed before validation so the CLI and
// MCP entry points behave the same when callers paste a value.
//
// An empty input (after trimming) returns an error; callers that
// treat empty as "no target version supplied" should check for the
// empty string before invoking this helper.
func ValidateTargetVersion(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("target version must not be empty")
	}
	canonical := strings.TrimPrefix(s, "v")
	parts := strings.Split(canonical, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return "", fmt.Errorf("target version %q is not MAJOR.MINOR or MAJOR.MINOR.PATCH", s)
	}
	for _, p := range parts {
		if p == "" {
			return "", fmt.Errorf("target version %q has an empty component", s)
		}
		// ParseUint rejects signs and non-numeric input in one step,
		// so "-1.4.0", "+1.4.0", and "25.x.0" all fail here. Atoi
		// would accept the signed forms, which is not the contract
		// we advertise.
		if _, err := strconv.ParseUint(p, 10, 32); err != nil {
			return "", fmt.Errorf("target version %q has non-numeric component %q", s, p)
		}
	}
	return canonical, nil
}

// VersionMismatchWarning compares the parser version compiled into the
// binary against the user-supplied target version and, when the major
// or minor components differ, returns a warning Error suitable for
// appending to Envelope.Errors. The bool return indicates whether a
// warning was produced; callers should append only when it is true.
//
// targetVersion is expected to already be in canonical form
// (post-ValidateTargetVersion). The silent-skip path is meant to
// tolerate an unresolvable parserVersion; a parse failure on
// targetVersion would indicate a caller contract violation but is
// also tolerated rather than panicked on, since it never makes sense
// to fail a SQL operation just because the version-mismatch check
// could not run.
//
// Comparison is on MAJOR.MINOR only — patch-level differences are
// noise for users who care about feature compatibility, not bug-fix
// releases. If either input cannot be parsed (e.g. parserVersion
// resolved to "unknown" in a development build), no warning is
// produced; callers should not punish users for an unresolvable
// bundled version.
//
// Known limitation (tracked separately): parserVersion today is the
// cockroachdb-parser Go-module version (e.g. "v0.26.2"), not the
// CockroachDB release version it implements. Until the parser ships
// a CRDB-version mapping, a user supplying --target-version 25.4.0
// will see "parser is v0.26 but target is v25.4" — technically
// correct under the implemented contract, but semantically a false
// positive. Stage 2/3 (#83/#85) will revisit how versions compare.
func VersionMismatchWarning(parserVersion, targetVersion string) (Error, bool) {
	parserMajorMinor, ok := majorMinor(parserVersion)
	if !ok {
		return Error{}, false
	}
	targetMajorMinor, ok := majorMinor(targetVersion)
	if !ok {
		return Error{}, false
	}
	if parserMajorMinor == targetMajorMinor {
		return Error{}, false
	}
	return Error{
		Code:     CodeTargetVersionMismatch,
		Severity: SeverityWarning,
		Message: fmt.Sprintf(
			"parser is v%s but target is v%s — results may differ",
			parserMajorMinor, targetMajorMinor,
		),
	}, true
}

// majorMinor extracts the MAJOR.MINOR prefix from a version string,
// tolerating a leading "v" and any number of additional dot-separated
// suffix components (e.g. patch, build metadata, pre-release tags).
// Returns ("", false) when the string does not start with two
// dot-separated unsigned-numeric components. Sign-prefixed parts like
// "-1" are rejected to match ValidateTargetVersion's contract.
func majorMinor(v string) (string, bool) {
	v = strings.TrimPrefix(v, "v")
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return "", false
	}
	if _, err := strconv.ParseUint(parts[0], 10, 32); err != nil {
		return "", false
	}
	if _, err := strconv.ParseUint(parts[1], 10, 32); err != nil {
		return "", false
	}
	return parts[0] + "." + parts[1], true
}
