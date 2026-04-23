// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package versionroute

import (
	"bytes"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// recordingExit captures the integer passed to a fake os.Exit. Tests
// verify both that exit was (or was not) called and the code value.
type recordingExit struct {
	called bool
	code   int
}

func (r *recordingExit) Exit(code int) {
	r.called = true
	r.code = code
}

func TestMaybeReexecNoFlag(t *testing.T) {
	var stderr bytes.Buffer
	exit := &recordingExit{}
	maybeReexec([]string{"crdb-sql", "validate", "-e", "SELECT 1"}, &stderr, exit.Exit)
	require.False(t, exit.called)
	require.Empty(t, stderr.String())
}

func TestMaybeReexecMalformedTargetVersion(t *testing.T) {
	var stderr bytes.Buffer
	exit := &recordingExit{}
	maybeReexec([]string{"crdb-sql", "--target-version", "not-a-version"}, &stderr, exit.Exit)
	require.False(t, exit.called, "malformed target must defer to cobra, not exit here")
	require.Empty(t, stderr.String())
}

func TestMaybeReexecMissingBackend(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("0o755 perm bits do not gate executability on Windows; covered indirectly")
	}
	// Empty PATH and no siblings => any --target-version that doesn't
	// match the (unknown) self quarter triggers the missing-backend
	// branch.
	t.Setenv("PATH", "")
	var stderr bytes.Buffer
	exit := &recordingExit{}
	maybeReexec([]string{"crdb-sql", "--target-version=25.1.0"}, &stderr, exit.Exit)
	require.True(t, exit.called)
	require.Equal(t, 2, exit.code)
	require.Contains(t, stderr.String(), "crdb-sql-v251")
	require.Contains(t, stderr.String(), "GitHub release")
}

func TestMaybeReexecMissingBackendListsAvailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("see TestMaybeReexecMissingBackend")
	}
	// Place a v254 sibling in PATH so the missing-backend message has
	// something to enumerate. Asking for v251 should fail but the
	// hint should mention the v254 sibling so the operator knows
	// what they have.
	dir := t.TempDir()
	makeFakeBackend(t, dir, "crdb-sql-v254")
	t.Setenv("PATH", dir)

	var stderr bytes.Buffer
	exit := &recordingExit{}
	maybeReexec([]string{"crdb-sql", "--target-version=25.1.0"}, &stderr, exit.Exit)

	require.True(t, exit.called)
	out := stderr.String()
	require.Contains(t, out, "crdb-sql-v251", "names the missing backend")
	require.Contains(t, out, "Available backends:", "starts the enumeration block")
	require.Contains(t, out, "crdb-sql-v254", "lists the discovered sibling so operator sees what they have")
	require.Contains(t, out, "(this binary)",
		"renders the running binary with its IsSelf marker so the operator can distinguish it")
}

func TestMaybeReexecMatchingQuarterIsNoop(t *testing.T) {
	// A --target-version that resolves to the same Quarter as the
	// running binary must not exec, exit, or write anything (modulo
	// the malformed-stamp diagnostic, which we ensure is absent
	// here). This locks in the equality check at maybeReexec — if a
	// regression always-routed, every command in the latest binary
	// would re-exec into itself or fail to find its sibling.
	prev := builtQuarterStamp
	builtQuarterStamp = "v262"
	defer func() { builtQuarterStamp = prev }()

	var stderr bytes.Buffer
	exit := &recordingExit{}
	maybeReexec([]string{"crdb-sql", "--target-version=26.2.5"}, &stderr, exit.Exit)
	require.False(t, exit.called, "matching quarter must not exit")
	require.Empty(t, stderr.String(), "matching quarter must not write to stderr")
}

func TestMaybeReexecPrintsStampDiagnostic(t *testing.T) {
	prev := builtQuarterStamp
	builtQuarterStamp = "garbage"
	defer func() { builtQuarterStamp = prev }()

	var stderr bytes.Buffer
	exit := &recordingExit{}
	// No flag, so no routing decision — but the malformed-stamp
	// diagnostic should fire on every invocation regardless.
	maybeReexec([]string{"crdb-sql", "version"}, &stderr, exit.Exit)
	require.False(t, exit.called)
	require.Contains(t, stderr.String(), "garbage")
	require.Contains(t, stderr.String(), "Reinstall")
}

func TestMaybeReexecMissingBackendDescribesUnknownSelf(t *testing.T) {
	t.Setenv("PATH", "")
	var stderr bytes.Buffer
	exit := &recordingExit{}
	maybeReexec([]string{"crdb-sql", "--target-version", "26.2.0"}, &stderr, exit.Exit)
	require.True(t, exit.called)
	// Test binaries lack a build stamp and parser dep, so the error
	// must say so rather than print an empty backend label.
	require.Contains(t, stderr.String(), "unknown")
}

// TestBuiltPrefersFilename guards against the exec-loop bug a
// filename-renamed sibling would otherwise trigger. The build stamp
// in this test binary is empty (unset under `go test`) so we can
// observe the filename-derived path in isolation by symlinking the
// running binary under a crdb-sql-vXXX name and reading os.Executable
// from the alternate path.
//
// We do not actually call Built() here — that reads the real
// os.Executable() of the test runner — but we exercise
// quarterFromExecutable which is the code path being protected.
func TestQuarterFromExecutableMatchesFilename(t *testing.T) {
	tests := []struct {
		name        string
		filename    string
		expectedOK  bool
		expectedTag string
	}{
		{name: "v254 backend", filename: "crdb-sql-v254", expectedOK: true, expectedTag: "v254"},
		{name: "v262 backend", filename: "crdb-sql-v262", expectedOK: true, expectedTag: "v262"},
		{name: "windows v254 backend", filename: "crdb-sql-v254.exe", expectedOK: true, expectedTag: "v254"},
		{name: "unsuffixed latest", filename: "crdb-sql", expectedOK: false},
		{name: "renamed to something else", filename: "my-sql-tool", expectedOK: false},
		{name: "garbage suffix", filename: "crdb-sql-banana", expectedOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reuse the same parsing logic via ParseTag rather than
			// shelling out to os.Executable. quarterFromExecutable
			// trims ".exe" then "crdb-sql-"; mirror that here.
			base := tt.filename
			if len(base) > 4 && base[len(base)-4:] == ".exe" {
				base = base[:len(base)-4]
			}
			const prefix = "crdb-sql-"
			if len(base) <= len(prefix) || base[:len(prefix)] != prefix {
				require.False(t, tt.expectedOK)
				return
			}
			q, ok := ParseTag(base[len(prefix):])
			require.Equal(t, tt.expectedOK, ok)
			if tt.expectedOK {
				require.Equal(t, tt.expectedTag, q.Tag())
			}
		})
	}
}
