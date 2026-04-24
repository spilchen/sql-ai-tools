// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cockroachtest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestStartReturnsErrBinaryNotFound exercises the binary-resolution
// failure path. Start is documented to return ErrBinaryNotFound when
// neither COCKROACH_BIN nor `cockroach` on $PATH resolves; this test
// pins COCKROACH_BIN to a non-existent path and clears PATH so the
// fallback also misses.
func TestStartReturnsErrBinaryNotFound(t *testing.T) {
	t.Setenv("COCKROACH_BIN", "/definitely/not/a/real/path/cockroach")
	t.Setenv("PATH", "")

	_, err := Start(context.Background())
	require.ErrorIs(t, err, ErrBinaryNotFound)
}

// TestStartReturnsErrBinaryNotFoundWithoutEnvVar covers the second
// half of the resolution flow: COCKROACH_BIN unset, no cockroach on
// PATH. We empty PATH so exec.LookPath cannot find anything.
func TestStartReturnsErrBinaryNotFoundWithoutEnvVar(t *testing.T) {
	t.Setenv("COCKROACH_BIN", "")
	t.Setenv("PATH", "")

	_, err := Start(context.Background())
	require.ErrorIs(t, err, ErrBinaryNotFound)
}

// TestStartTimesOutWhenURLNeverAppears verifies that Start gives up
// after WithStartTimeout if the binary runs but never writes the
// listening-url-file. The fake binary just sleeps; Start should
// return a timeout error and clean up the partial process.
func TestStartTimesOutWhenURLNeverAppears(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake-binary harness uses /bin/sh; not portable to Windows")
	}
	fake := writeShellBinary(t, "exec sleep 30")
	t.Setenv("COCKROACH_BIN", fake)

	_, err := Start(context.Background(), WithStartTimeout(300*time.Millisecond))
	require.Error(t, err)
	require.Contains(t, err.Error(), "timed out")
}

// TestStartDetectsPrematureExit verifies the short-circuit when the
// fake binary exits before writing the URL file. Start should return
// promptly (well under the configured timeout) rather than polling
// for the full duration.
func TestStartDetectsPrematureExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake-binary harness uses /bin/sh; not portable to Windows")
	}
	fake := writeShellBinary(t, "exit 0")
	t.Setenv("COCKROACH_BIN", fake)

	deadline := time.Now().Add(5 * time.Second)
	_, err := Start(context.Background(), WithStartTimeout(30*time.Second))
	require.Error(t, err)
	require.Contains(t, err.Error(), "exited before writing listening URL")
	require.True(t, time.Now().Before(deadline),
		"premature-exit detection should be near-instant, not wait for the timeout")
}

// TestSharedReportsMissingBinary covers Shared's missing-binary
// behavior across the full CRDB_INTEGRATION_OPTIONAL contract. Unset
// and falsy values must Fatal (with a message naming every recovery
// knob); strconv.ParseBool-truthy values must Skip; and an
// unparseable value must Fatal rather than silently default to "off",
// because a typo here is exactly the silent-skip footgun the package
// is trying to remove.
func TestSharedReportsMissingBinary(t *testing.T) {
	tests := []struct {
		name             string
		optionalEnv      string
		expectedSkipped  bool
		expectedFailed   bool
		expectedMsgParts []string
	}{
		{
			name:           "fatal by default (unset)",
			optionalEnv:    "",
			expectedFailed: true,
			expectedMsgParts: []string{
				"$PATH", "COCKROACH_BIN", "CRDB_TEST_DSN", "CRDB_INTEGRATION_OPTIONAL=1",
			},
		},
		{
			name:           "fatal when explicitly disabled",
			optionalEnv:    "0",
			expectedFailed: true,
			expectedMsgParts: []string{
				"$PATH", "COCKROACH_BIN", "CRDB_TEST_DSN", "CRDB_INTEGRATION_OPTIONAL=1",
			},
		},
		{
			name:             "fatal on unparseable value",
			optionalEnv:      "please",
			expectedFailed:   true,
			expectedMsgParts: []string{`CRDB_INTEGRATION_OPTIONAL="please"`, "not a boolean"},
		},
		{
			name:             "skip when opt-in set to 1",
			optionalEnv:      "1",
			expectedSkipped:  true,
			expectedMsgParts: []string{"binary not found", `CRDB_INTEGRATION_OPTIONAL="1"`},
		},
		{
			name:             "skip when opt-in set to true",
			optionalEnv:      "true",
			expectedSkipped:  true,
			expectedMsgParts: []string{"binary not found", `CRDB_INTEGRATION_OPTIONAL="true"`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("COCKROACH_BIN", "/definitely/not/a/real/path/cockroach")
			t.Setenv("PATH", "")
			t.Setenv("CRDB_TEST_DSN", "")
			t.Setenv("CRDB_INTEGRATION_OPTIONAL", tc.optionalEnv)
			resetSharedState()

			rec := &recordingTB{TB: t}
			c := Shared(rec)
			require.Nil(t, c)
			require.Equal(t, tc.expectedSkipped, rec.skipped, "skip outcome")
			require.Equal(t, tc.expectedFailed, rec.failed, "fatal outcome")

			msg := rec.skipMsg
			if rec.failed {
				msg = rec.failMsg
			}
			for _, part := range tc.expectedMsgParts {
				require.Contains(t, msg, part)
			}
		})
	}
}

// TestSharedCachedFailureReplaysContract pins the regression-prevention
// claim of the centralized error reporter: once a Shared call has
// failed and cached an error, every subsequent Shared call in the
// same test binary replays the same Skip-vs-Fatal decision against
// the current CRDB_INTEGRATION_OPTIONAL setting. Without this, a
// future refactor could let test #1 fatal while test #2 silently
// returns nil — the original bug.
func TestSharedCachedFailureReplaysContract(t *testing.T) {
	t.Setenv("COCKROACH_BIN", "/definitely/not/a/real/path/cockroach")
	t.Setenv("PATH", "")
	t.Setenv("CRDB_TEST_DSN", "")
	t.Setenv("CRDB_INTEGRATION_OPTIONAL", "")
	resetSharedState()

	first := &recordingTB{TB: t}
	require.Nil(t, Shared(first))
	require.True(t, first.failed, "first call must Fatal when opt-in is unset")

	second := &recordingTB{TB: t}
	require.Nil(t, Shared(second))
	require.True(t, second.failed, "cached failure must Fatal again on the second call")

	t.Setenv("CRDB_INTEGRATION_OPTIONAL", "1")
	third := &recordingTB{TB: t}
	require.Nil(t, Shared(third))
	require.True(t, third.skipped, "cached failure must honor a now-set opt-in on later calls")
	require.False(t, third.failed, "cached failure must not Fatal once opt-in is set")
}

// TestSharedCachedGenericFailureAlwaysFatals pins the second
// regression-prevention claim: a generic Start failure (anything
// other than ErrBinaryNotFound) cached on the first call must always
// Fatal on subsequent calls — never Skip — even when
// CRDB_INTEGRATION_OPTIONAL=1 is set. The CI escape hatch is for
// missing-binary cases only; a real cluster-startup bug would
// otherwise silently skip on every test after the first.
func TestSharedCachedGenericFailureAlwaysFatals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake-binary harness uses /bin/sh; not portable to Windows")
	}
	fake := writeShellBinary(t, "exit 0")
	t.Setenv("COCKROACH_BIN", fake)
	t.Setenv("CRDB_TEST_DSN", "")
	t.Setenv("CRDB_INTEGRATION_OPTIONAL", "1")
	resetSharedState()

	first := &recordingTB{TB: t}
	require.Nil(t, Shared(first))
	require.True(t, first.failed, "first call must Fatal on generic Start failure")
	require.Contains(t, first.failMsg, "failed to start cluster")

	second := &recordingTB{TB: t}
	require.Nil(t, Shared(second))
	require.True(t, second.failed,
		"cached generic failure must Fatal even with opt-in set; otherwise CI silently skips real bugs")
	require.False(t, second.skipped)
	require.Contains(t, second.failMsg, "failed to start cluster")
}

// TestSharedHonorsCRDBTestDSN pins the documented escape hatch: when
// CRDB_TEST_DSN is set, Shared returns a Cluster wrapping the env
// value and never spawns a subprocess (even if the binary lookup
// would have failed). Stop must be a no-op for the bypass shape.
func TestSharedHonorsCRDBTestDSN(t *testing.T) {
	const dsn = "postgres://example/dummy"
	t.Setenv("COCKROACH_BIN", "/definitely/not/a/real/path/cockroach")
	t.Setenv("PATH", "")
	t.Setenv("CRDB_TEST_DSN", dsn)
	resetSharedState()

	rec := &recordingTB{TB: t}
	c := Shared(rec)
	require.NotNil(t, c)
	require.False(t, rec.skipped, "Shared must not Skip when CRDB_TEST_DSN is set")
	require.False(t, rec.failed, "Shared must not Fatal when CRDB_TEST_DSN is set")
	require.Equal(t, dsn, c.DSN)
	require.Empty(t, c.Logs(), "bypass cluster has no subprocess to capture from")
	require.NoError(t, c.Stop(), "Stop on a CRDB_TEST_DSN cluster must be a no-op")
	require.NoError(t, c.Stop(), "Stop must remain a no-op on repeat invocation")
}

// TestStopIsIdempotentAndCleansUpTmpdir pins two related contracts in
// one test: Stop is safe to call repeatedly (sync.Once), and the
// tmpdir is removed after the first successful Stop. Uses a fake
// binary that writes a fabricated URL then sleeps so Start succeeds
// without needing a real cockroach.
func TestStopIsIdempotentAndCleansUpTmpdir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake-binary harness uses /bin/sh; not portable to Windows")
	}
	// The harness invokes the binary with `demo --background ...
	// --listening-url-file=<path> ...`. Parse the path from $@ by
	// finding the --listening-url-file= flag (long-form, single arg)
	// and write a fake DSN to it before sleeping.
	body := `for arg in "$@"; do
  case "$arg" in
    --listening-url-file=*)
      printf 'postgres://fake@127.0.0.1:1/defaultdb\n' > "${arg#--listening-url-file=}"
      ;;
  esac
done
exec sleep 30`
	fake := writeShellBinary(t, body)
	t.Setenv("COCKROACH_BIN", fake)

	c, err := Start(context.Background(), WithStartTimeout(5*time.Second))
	require.NoError(t, err)
	require.NotEmpty(t, c.DSN)

	tmpDir := c.tmpDir
	require.DirExists(t, tmpDir)

	require.NoError(t, c.Stop())
	require.NoDirExists(t, tmpDir, "tmpdir must be removed after Stop")

	require.NoError(t, c.Stop(), "Stop must be idempotent")
}

// writeShellBinary creates an executable shell script in a per-test
// temp dir and returns its absolute path. The body is appended after
// the `#!/bin/sh` shebang. Suitable for use as a stand-in
// COCKROACH_BIN that simulates a hung or misbehaving binary.
func writeShellBinary(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-cockroach-"+strconv.Itoa(os.Getpid()))
	contents := "#!/bin/sh\n" + body + "\n"
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o755))
	return path
}

// resetSharedState clears the package-level shared-cluster singleton.
// Tests use it to avoid order-dependence when exercising Shared.
func resetSharedState() {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	sharedCluster = nil
	sharedErr = nil
	sharedStarted = false
}

// recordingTB wraps a testing.TB and intercepts Skipf, Fatalf, and
// Helper so a unit test can assert on Shared's outcome without
// actually skipping or failing itself. Skipf/Fatalf record the
// message and return instead of invoking runtime.Goexit, so the
// caller can keep inspecting the recorder after Shared returns nil
// (production callers never observe that nil — the standard
// *testing.T halts before the return). Helper is a no-op so the
// embedded TB does not record this wrapper's call frame as the
// helper boundary. Everything else delegates to the embedded value.
type recordingTB struct {
	testing.TB
	skipped bool
	skipMsg string
	failed  bool
	failMsg string
}

func (r *recordingTB) Skipf(format string, args ...any) {
	r.skipped = true
	r.skipMsg = fmt.Sprintf(format, args...)
}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.failed = true
	r.failMsg = fmt.Sprintf(format, args...)
}

func (r *recordingTB) Helper() {}
