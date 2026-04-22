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

// TestSharedSkipsWhenNoBinary verifies Shared's t.Skipf path. The
// shared-state reset keeps the test independent of execution order
// inside this package.
func TestSharedSkipsWhenNoBinary(t *testing.T) {
	t.Setenv("COCKROACH_BIN", "/definitely/not/a/real/path/cockroach")
	t.Setenv("PATH", "")
	t.Setenv("CRDB_TEST_DSN", "")
	resetSharedState()

	rec := &recordingTB{TB: t}
	c := Shared(rec)
	require.Nil(t, c)
	require.True(t, rec.skipped, "expected Shared to call Skipf when binary is absent")
	require.Contains(t, rec.skipMsg, "binary not found")
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

// recordingTB wraps a testing.TB and intercepts Skipf so a unit test
// can assert on the skip outcome without actually skipping itself.
// All other testing.TB methods delegate to the embedded value.
type recordingTB struct {
	testing.TB
	skipped bool
	skipMsg string
}

func (r *recordingTB) Skipf(format string, args ...any) {
	r.skipped = true
	r.skipMsg = fmt.Sprintf(format, args...)
}

func (r *recordingTB) Helper() {}
