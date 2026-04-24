// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package cockroachtest spins up an in-memory single-node CockroachDB
// cluster (via `cockroach demo --background`) for use by integration
// tests in this module. It is the only place in the codebase that
// shells out to a real cockroach binary; integration tests under
// `//go:build integration` consume it through Shared / RunTests.
//
// Lifecycle:
//
//   - Start launches a demo cluster, blocks until it has written its
//     listening URL, and returns a *Cluster whose DSN field is a usable
//     postgres URL. Stop sends SIGINT, waits up to 10s for graceful
//     exit, then escalates to SIGKILL. On a Start failure the harness
//     has already killed any partially-started process and removed the
//     temp directory before returning.
//
//   - Shared and RunTests provide the per-test-binary pattern used by
//     integration test packages: one cluster is started on the first
//     Shared call and torn down by RunTests when the test binary exits.
//     Shared fails the test (rather than skipping) when no cluster is
//     reachable; see "Binary resolution" below for the opt-out.
//
// Binary resolution (in order):
//
//  1. COCKROACH_BIN environment variable.
//  2. `cockroach` on $PATH.
//
// If neither resolves to an executable file, Start returns
// ErrBinaryNotFound. Shared turns that into t.Fatalf by default so a
// silent skip can never masquerade as a passing integration test;
// environments that legitimately lack a cockroach binary (today: the
// GitHub Actions CI job) opt back into the old skip behavior by
// setting CRDB_INTEGRATION_OPTIONAL=1.
//
// CRDB_TEST_DSN bypass: when CRDB_TEST_DSN is set, Shared returns a
// Cluster with only DSN populated and a no-op Stop. This lets a
// developer point integration tests at an already-running cluster
// without paying the demo startup cost.
package cockroachtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Default tuning constants. defaultStartTimeout is overridable via
// WithStartTimeout; defaultStopTimeout (the SIGINT-grace deadline
// before SIGKILL escalation) and urlPollInterval are not exposed —
// they have no plausible test-driven reason to differ across callers.
const (
	defaultStartTimeout = 30 * time.Second
	defaultStopTimeout  = 10 * time.Second
	urlPollInterval     = 100 * time.Millisecond
)

// ErrBinaryNotFound is returned by Start when no cockroach binary can
// be located via COCKROACH_BIN or $PATH. Shared turns this into
// t.Fatalf by default so missing-binary cases fail loudly, or into
// t.Skipf when CRDB_INTEGRATION_OPTIONAL holds a strconv.ParseBool-truthy
// value (the CI escape hatch — falsy or unparseable values still Fatal,
// so a typo can't accidentally enable skipping). Shared callers also
// have CRDB_TEST_DSN as a third option (point at an already-running
// cluster); Start itself does not honor that var because its job is
// to spawn a process, not to wrap an existing one.
var ErrBinaryNotFound = errors.New(
	"cockroach binary not found (set COCKROACH_BIN or put cockroach on PATH)")

// Cluster represents a running `cockroach demo` instance owned by the
// test process. Construct with Start; release with Stop. The exported
// DSN field is the postgres URL the demo cluster wrote to its
// listening-url-file and is safe to read after Start returns.
//
// A Cluster is single-use: once Stop has been called, the cluster
// cannot be restarted. Tests that need multiple clusters call Start
// multiple times (each invocation gets its own tempdir and process).
//
// Concurrency: a single monitor goroutine started by Start owns
// cmd.Wait() and signals process exit via the waitDone channel. All
// other code paths (premature-exit detection during startup, Stop's
// graceful-shutdown wait) observe exit by selecting on waitDone, so
// there is exactly one Wait caller and no double-wait race.
type Cluster struct {
	// DSN is the postgres connection string for the running demo node.
	// Populated by Start before it returns; never modified afterward.
	DSN string

	// cmd is the demo subprocess. Owned by Start, the monitor
	// goroutine, and Stop; integration tests do not interact with it.
	cmd *exec.Cmd

	// logBuf captures combined stdout+stderr from the demo. It is a
	// concurrency-safe wrapper because cmd's internal goroutines write
	// to it while Logs() may read concurrently from a test goroutine.
	logBuf *lockedBuf

	// tmpDir holds the URL file and store directory. Removed by Stop.
	tmpDir string

	// waitDone closes when the cmd.Wait monitor goroutine returns.
	// waitErr holds the Wait result; the write happens-before the
	// close, so any reader that has observed waitDone closed may
	// safely read waitErr. Used by Start's failure path to surface
	// the subprocess exit code/signal in the wrapped error
	// ("exit status 1" vs "signal: killed" tells you whether crdb
	// crashed, the OOM killer killed it, or a flag was rejected).
	waitDone chan struct{}
	waitErr  error

	// stopOnce ensures Stop is idempotent: once a teardown has been
	// performed, repeat calls are no-ops returning the original error.
	stopOnce sync.Once
	stopErr  error
}

// lockedBuf is a concurrency-safe bytes.Buffer wrapper. exec.Cmd's
// internal stdout/stderr-copy goroutines write to it; Logs() reads
// from a test goroutine. bytes.Buffer alone would race.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// startOpts holds the resolved Option values for Start.
type startOpts struct {
	startTimeout time.Duration
	extraArgs    []string
	secure       bool
}

// Option configures Start.
type Option interface {
	apply(*startOpts)
}

type optionFunc func(*startOpts)

func (f optionFunc) apply(o *startOpts) { f(o) }

// WithStartTimeout overrides the default 30s timeout Start uses
// while polling for the demo cluster's listening-url-file. This
// governs only URL-file appearance, not full SQL readiness — that
// readiness is the caller's concern (typically a Ping retry loop).
// Demo startup on a loaded CI machine can be slower than 30s; bump
// this if Start times out.
func WithStartTimeout(d time.Duration) Option {
	return optionFunc(func(o *startOpts) { o.startTimeout = d })
}

// WithExtraArgs appends additional command-line arguments to the
// `cockroach demo` invocation, after the harness's own flags. Use
// this for tests that need a non-default demo configuration; do not
// pass any flag that Start already sets (see the args slice in
// Start). Multiple WithExtraArgs calls accumulate; arguments appear
// in call order.
func WithExtraArgs(args ...string) Option {
	return optionFunc(func(o *startOpts) {
		o.extraArgs = append(o.extraArgs, args...)
	})
}

// WithSecure drops `--insecure` from the cockroach demo invocation.
// In secure mode demo provisions a self-signed CA and a node
// certificate into a tempdir, then writes a TLS-enabled DSN whose
// query string includes at least `sslmode` and `sslrootcert` pointing
// at the demo-issued CA. Demo authenticates the SQL session with a
// generated password rather than a client certificate, so the DSN
// does not carry `sslcert`/`sslkey` — tests exercising the client-cert
// auth path need their own `cockroach cert create-*` setup.
//
// Secure clusters cannot be served by Shared, which caches an
// insecure cluster — call Start directly and Stop in defer/cleanup.
func WithSecure() Option {
	return optionFunc(func(o *startOpts) { o.secure = true })
}

// Start launches an in-memory single-node demo cluster in the
// background. It blocks until the cluster has written its listening
// URL to a private temp file (default 30s timeout, configurable via
// WithStartTimeout). On success the returned Cluster's DSN is a valid
// postgres URL and the caller is responsible for invoking Stop. On
// failure, any partially-started process has been killed and the
// temp directory removed before Start returns.
//
// Start honors COCKROACH_BIN; if unset, it looks up `cockroach` on
// $PATH. Returns ErrBinaryNotFound when neither resolves.
func Start(ctx context.Context, opts ...Option) (*Cluster, error) {
	resolved := startOpts{startTimeout: defaultStartTimeout}
	for _, o := range opts {
		o.apply(&resolved)
	}

	binary, err := resolveBinary()
	if err != nil {
		return nil, err
	}

	tmpDir, err := os.MkdirTemp("", "cockroachtest-")
	if err != nil {
		return nil, fmt.Errorf("create tempdir: %w", err)
	}
	urlPath := filepath.Join(tmpDir, "listen.url")

	args := []string{
		"demo",
		"--background",
		"--no-example-database",
		"--disable-demo-license",
		"--listening-url-file=" + urlPath,
		"--sql-port=0",
		"--http-port=0",
	}
	if !resolved.secure {
		args = append(args, "--insecure")
	}
	args = append(args, resolved.extraArgs...)

	logBuf := &lockedBuf{}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout = logBuf
	cmd.Stderr = logBuf

	if err := cmd.Start(); err != nil {
		startErr := fmt.Errorf("start cockroach demo: %w", err)
		if rmErr := os.RemoveAll(tmpDir); rmErr != nil {
			return nil, errors.Join(startErr, fmt.Errorf("remove tmpdir %q: %w", tmpDir, rmErr))
		}
		return nil, startErr
	}

	c := &Cluster{
		cmd:      cmd,
		logBuf:   logBuf,
		tmpDir:   tmpDir,
		waitDone: make(chan struct{}),
	}

	// Single Wait owner: the monitor goroutine started here is the
	// only caller of cmd.Wait(). waitForURL and Stop observe exit by
	// selecting on waitDone, never by calling Wait themselves.
	go func() {
		c.waitErr = cmd.Wait()
		close(c.waitDone)
	}()

	dsn, err := waitForURL(ctx, urlPath, c.waitDone, resolved.startTimeout)
	if err != nil {
		// Kill the partially-started process (if it is still alive)
		// and clean up the tmpdir so callers don't have to. The
		// wrapped error captures the process exit and the full
		// subprocess log so a CI failure is debuggable from the test
		// output alone — without these, "exited before writing
		// listening URL" is uselessly opaque.
		stopErr := c.Stop()
		// Stop synchronously drains the monitor goroutine on every
		// reachable path here (cmd.Start succeeded, so the monitor
		// is running and shutdown waits on waitDone), but we
		// re-observe the channel close explicitly so the read of
		// c.waitErr does not depend on shutdown's internal drain
		// remaining unchanged across future refactors.
		<-c.waitDone
		exitInfo := "exit 0"
		if c.waitErr != nil {
			exitInfo = c.waitErr.Error()
		}
		wrapped := fmt.Errorf("%w (process exit: %s)\nDemo logs:\n%s", err, exitInfo, c.Logs())
		if stopErr != nil {
			wrapped = errors.Join(wrapped, fmt.Errorf("teardown after Start failure: %w", stopErr))
		}
		return nil, wrapped
	}
	c.DSN = dsn
	return c, nil
}

// Stop sends SIGINT to the demo process and waits up to 10s for it to
// exit. If the process does not exit in time, Stop sends SIGKILL and
// the returned error reports that the graceful shutdown deadline was
// exceeded. Stop also removes the cluster's tempdir; either failure
// (shutdown or tmpdir removal) is surfaced via errors.Join so neither
// is silently lost. Stop is idempotent: subsequent calls return the
// original error without re-executing the teardown. The captured
// stdout/stderr remains accessible via Logs() after Stop returns.
func (c *Cluster) Stop() error {
	c.stopOnce.Do(func() {
		shutdownErr := c.shutdown()
		var rmErr error
		if c.tmpDir != "" {
			if err := os.RemoveAll(c.tmpDir); err != nil {
				rmErr = fmt.Errorf("remove tmpdir %q: %w", c.tmpDir, err)
			}
		}
		c.stopErr = errors.Join(shutdownErr, rmErr)
	})
	return c.stopErr
}

// Logs returns a snapshot of the demo process's combined stdout and
// stderr. For clusters constructed via the CRDB_TEST_DSN bypass
// (which never spawns a subprocess) it returns an empty string with
// no special marker; callers diagnosing a failure should treat empty
// output as "no subprocess to capture from". When the buffer is
// present, Logs is safe to call concurrently with the running
// subprocess: writes from the cmd's stdout/stderr-copy goroutines
// are mutex-serialized in lockedBuf.
func (c *Cluster) Logs() string {
	if c.logBuf == nil {
		return ""
	}
	return c.logBuf.String()
}

// shutdown signals the demo process and waits for exit. SIGINT first
// with a defaultStopTimeout grace period; SIGKILL after. Returns nil
// when the process is already gone or was never spawned. Returns a
// non-nil error when the SIGINT deadline expires, capturing the
// SIGKILL outcome so a hung crdb is debuggable rather than silently
// papered over.
func (c *Cluster) shutdown() error {
	if c.cmd == nil || c.cmd.Process == nil {
		return nil
	}

	// If the monitor goroutine has already observed exit (premature
	// crash, or a prior Signal), there is nothing to signal — just
	// return.
	select {
	case <-c.waitDone:
		return nil
	default:
	}

	// Phase 1: SIGINT and bounded grace period. Demo with --background
	// is documented to shut down on SIGINT/SIGTERM; the SIGKILL path
	// is a safety net for hung processes, not the expected case.
	if err := c.cmd.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("signal cockroach demo: %w", err)
	}

	select {
	case <-c.waitDone:
		// A non-zero exit from a SIGINT'd cockroach is expected; we do
		// not bubble it up as a test failure.
		return nil
	case <-time.After(defaultStopTimeout):
		killErr := c.cmd.Process.Kill()
		<-c.waitDone
		if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			return fmt.Errorf("cockroach demo did not exit on SIGINT within %s; SIGKILL also failed: %w", defaultStopTimeout, killErr)
		}
		return fmt.Errorf("cockroach demo did not exit on SIGINT within %s; sent SIGKILL", defaultStopTimeout)
	}
}

// resolveBinary picks the cockroach binary using the documented
// COCKROACH_BIN -> PATH fallback. Returns ErrBinaryNotFound when no
// candidate resolves to an executable file.
func resolveBinary() (string, error) {
	if envBin := os.Getenv("COCKROACH_BIN"); envBin != "" {
		info, err := os.Stat(envBin)
		if err != nil || info.IsDir() {
			return "", fmt.Errorf("%w: COCKROACH_BIN=%q", ErrBinaryNotFound, envBin)
		}
		return envBin, nil
	}
	path, err := exec.LookPath("cockroach")
	if err != nil {
		return "", ErrBinaryNotFound
	}
	return path, nil
}

// waitForURL polls urlPath until the demo process has written a
// non-empty URL or the timeout elapses. If the process exits before
// writing the file (e.g., flag rejection), waitForURL returns
// promptly via the procExited channel rather than waiting out the
// full timeout. The caller passes the Cluster's waitDone channel; the
// Wait owner is the goroutine started in Start.
func waitForURL(ctx context.Context, urlPath string, procExited <-chan struct{}, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		// Read the file unconditionally; an empty/missing file is the
		// "not ready yet" signal and we just keep polling.
		if data, err := os.ReadFile(urlPath); err == nil {
			url := strings.TrimSpace(string(data))
			if url != "" {
				return url, nil
			}
		}

		select {
		case <-ctx.Done():
			return "", fmt.Errorf("waiting for cockroach demo URL: %w", ctx.Err())
		case <-procExited:
			return "", errors.New("cockroach demo exited before writing listening URL")
		case <-time.After(urlPollInterval):
			if time.Now().After(deadline) {
				return "", fmt.Errorf("timed out after %s waiting for cockroach demo to write %s", timeout, urlPath)
			}
		}
	}
}

// Per-test-binary shared cluster. The first Shared call starts a
// cluster (or resolves CRDB_TEST_DSN); subsequent calls in the same
// test binary return the same instance. RunTests tears it down.
var (
	sharedMu      sync.Mutex
	sharedCluster *Cluster
	sharedErr     error
	sharedStarted bool
)

// integrationOptionalEnv is the opt-in env var that converts Shared's
// missing-binary fatal into a skip. It exists for one reason: the
// GitHub Actions CI job does not install cockroach and would otherwise
// fail every integration-tagged test. Anywhere else (developer
// laptops, fresh checkouts, ad-hoc CI), leaving it unset means missing
// binaries surface as loud test failures instead of invisible skips.
//
// The value is parsed by strconv.ParseBool — see that function for the
// full grammar. Truthy values (e.g. "1", "true") enable the skip;
// falsy ones (e.g. "0", "false") disable it. An unparseable value
// (e.g. "yes", "on", "1\n") is itself a Fatal — silently treating an
// unrecognized value as "off" would just relocate the silent-skip
// footgun this package is trying to remove.
const integrationOptionalEnv = "CRDB_INTEGRATION_OPTIONAL"

// missingBinaryMsg is the message Shared uses when it cannot reach a
// cluster and CRDB_INTEGRATION_OPTIONAL is unset. It enumerates every
// knob that would make the test runnable so a developer hitting the
// failure for the first time has a complete recovery menu in front of
// them.
const missingBinaryMsg = "cockroachtest: no cockroach binary found and CRDB_TEST_DSN unset; " +
	"install cockroach on $PATH, set COCKROACH_BIN, set CRDB_TEST_DSN=postgresql://..., " +
	"or export CRDB_INTEGRATION_OPTIONAL=1 to skip"

// Shared returns the per-test-binary cluster, starting one on the
// first call. CRDB_TEST_DSN takes precedence: if set at first-call
// time, Shared returns a Cluster whose DSN is the env value and
// whose Stop is a no-op.
//
// Env-read timing: CRDB_TEST_DSN and the binary-resolution variables
// (COCKROACH_BIN, $PATH) are read only on the first call; the result
// is cached. CRDB_INTEGRATION_OPTIONAL is the exception — it is
// re-read on every call so a test that flips the opt-in mid-run gets
// the new behavior on subsequent calls, not the value captured at
// first-call time.
//
// Behavior on missing binary: when neither CRDB_TEST_DSN nor a
// resolvable cockroach binary is available, Shared calls t.Fatalf by
// default. Setting CRDB_INTEGRATION_OPTIONAL to a strconv.ParseBool
// truthy value (e.g. "1", "true") converts the failure into t.Skipf —
// this is the escape hatch for environments (today: the GitHub
// Actions job) that legitimately have no cockroach installed. A
// falsy or unparseable value Fatals just like the unset case, so a
// typo can't accidentally enable skipping.
//
// Behavior on cached failure: a Shared call that follows a failed
// Shared call replays the Skip/Fatal decision against the *current*
// CRDB_INTEGRATION_OPTIONAL value, but only for ErrBinaryNotFound. A
// generic Start failure cached on the first call always Fatals —
// never skips — because CRDB_INTEGRATION_OPTIONAL is meant only for
// the missing-binary case.
//
// On a real *testing.T, both Fatalf and Skipf invoke runtime.Goexit,
// so the nil return is only observable to test doubles that override
// them (e.g. the recordingTB used in cluster_test.go). Production
// callers can rely on Shared either returning a usable *Cluster or
// never returning.
//
// Tests using Shared must wire their TestMain through RunTests so the
// cluster is cleanly torn down at exit.
func Shared(t testing.TB) *Cluster {
	t.Helper()
	sharedMu.Lock()
	defer sharedMu.Unlock()

	if sharedStarted {
		if sharedErr != nil {
			reportCachedErr(t, sharedErr)
			return nil
		}
		return sharedCluster
	}
	sharedStarted = true

	if dsn := os.Getenv("CRDB_TEST_DSN"); dsn != "" {
		sharedCluster = &Cluster{DSN: dsn}
		return sharedCluster
	}

	c, err := Start(context.Background())
	if errors.Is(err, ErrBinaryNotFound) {
		sharedErr = err
		reportMissingBinary(t, err)
		return nil
	}
	if err != nil {
		sharedErr = err
		t.Fatalf("cockroachtest: failed to start cluster: %v", err)
		return nil
	}
	sharedCluster = c
	return sharedCluster
}

// reportMissingBinary handles the ErrBinaryNotFound outcome at the
// first Shared call. It honors the CRDB_INTEGRATION_OPTIONAL escape
// hatch (skip) on parseable-truthy values and Fatalf otherwise. An
// unparseable value is itself a Fatal — relaying "I don't know what
// you meant" as a skip would just re-introduce the silent-skip
// footgun this package is trying to eliminate.
func reportMissingBinary(t testing.TB, err error) {
	t.Helper()
	raw := os.Getenv(integrationOptionalEnv)
	if raw == "" {
		t.Fatalf("%s (underlying: %v)", missingBinaryMsg, err)
		return
	}
	optIn, perr := strconv.ParseBool(raw)
	if perr != nil {
		t.Fatalf("%s=%q is not a boolean (%v); %s",
			integrationOptionalEnv, raw, perr, missingBinaryMsg)
		return
	}
	if !optIn {
		t.Fatalf("%s (underlying: %v)", missingBinaryMsg, err)
		return
	}
	t.Skipf("cockroachtest: %v; %s=%q set, skipping", err, integrationOptionalEnv, raw)
}

// reportCachedErr handles a Shared call that follows an earlier
// Shared call which already failed. It discriminates by error type:
// ErrBinaryNotFound flows back through reportMissingBinary so the CI
// escape hatch still applies, but a generic Start failure (port
// collision, premature exit, tempdir trouble, etc.) is always Fatal.
// Routing every cached error through the missing-binary helper would
// make CRDB_INTEGRATION_OPTIONAL=1 silently skip real startup
// failures on every test after the first — the exact pattern this
// commit set out to eliminate.
func reportCachedErr(t testing.TB, err error) {
	t.Helper()
	if errors.Is(err, ErrBinaryNotFound) {
		reportMissingBinary(t, err)
		return
	}
	t.Fatalf("cockroachtest: failed to start cluster: %v", err)
}

// RunTests is the TestMain helper for integration test packages. It
// runs the standard test suite via m.Run and then tears down any
// shared cluster that Shared created. Call from each integration test
// package as:
//
//	func TestMain(m *testing.M) { cockroachtest.RunTests(m) }
//
// RunTests calls os.Exit, mirroring the standard TestMain contract.
// Teardown errors are written to stderr before os.Exit because
// m.Run has already returned and t.Log is unavailable; without this,
// a hung-shutdown or tmpdir-cleanup failure would silently exit 0.
func RunTests(m *testing.M) {
	code := m.Run()
	sharedMu.Lock()
	c := sharedCluster
	sharedMu.Unlock()
	if c != nil {
		if err := c.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "cockroachtest: shared cluster teardown failed: %v\n", err)
		}
	}
	os.Exit(code)
}
