// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package proxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/versionroute"
)

// fakeMCPClient is the test double for mcp-go's *client.Client.
// Each instance represents one warm sibling. The fake records every
// CallTool entry/exit so tests can assert on serialization, and
// surfaces errors injected by the test for the transport-failure
// and broken-Close branches of PoolRouter.
type fakeMCPClient struct {
	// callErr, when non-nil, is returned from every CallTool —
	// drives the dead-child recovery test.
	callErr error

	// callDelay, when non-zero, blocks CallTool for this long so the
	// serialization test can prove that two concurrent Dispatches
	// queue rather than overlap.
	callDelay time.Duration

	// callEntered, when non-nil, is closed exactly once when
	// CallTool first enters. Lets tests prove "this point was
	// reached" without timing assumptions.
	callEntered chan struct{}

	// callRelease, when non-nil, blocks CallTool until something
	// is sent on (or the channel is closed). Lets tests pin a
	// CallTool in flight while they perform another action and
	// then deterministically release it.
	callRelease chan struct{}

	// closeErr, when non-nil, is returned from Close.
	closeErr error

	mu          sync.Mutex
	calls       int
	inFlight    int
	maxInFlight int
	closed      bool
	closeCount  int
}

func (f *fakeMCPClient) CallTool(
	ctx context.Context, _ mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	f.mu.Lock()
	f.calls++
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	delay := f.callDelay
	entered := f.callEntered
	release := f.callRelease
	f.mu.Unlock()
	if entered != nil {
		// Signal exactly once across the lifetime of the fake;
		// guard against repeat entry (would panic on close).
		f.mu.Lock()
		if f.callEntered == entered {
			close(entered)
			f.callEntered = nil
		}
		f.mu.Unlock()
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			f.mu.Lock()
			f.inFlight--
			f.mu.Unlock()
			return nil, ctx.Err()
		}
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			f.mu.Lock()
			f.inFlight--
			f.mu.Unlock()
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	f.inFlight--
	err := f.callErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{}, nil
}

func (f *fakeMCPClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCount++
	f.closed = true
	return f.closeErr
}

func (f *fakeMCPClient) snapshot() (calls int, maxInFlight int, closes int, closed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.maxInFlight, f.closeCount, f.closed
}

// fakeSpawner returns a spawnFunc that hands out fakeMCPClients per
// quarter. Use makeFakeSpawner to construct one with a controlled
// roster; the returned function records every spawn so tests can
// assert spawn counts.
type spawnRecord struct {
	mu      sync.Mutex
	clients map[versionroute.Quarter][]*fakeMCPClient
	errs    map[versionroute.Quarter]error
}

func newSpawnRecord() *spawnRecord {
	return &spawnRecord{
		clients: map[versionroute.Quarter][]*fakeMCPClient{},
		errs:    map[versionroute.Quarter]error{},
	}
}

// failNext arms the next spawn for want to return err exactly once.
func (r *spawnRecord) failNext(want versionroute.Quarter, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs[want] = err
}

// spawned returns the slice of clients handed out for want, in
// spawn order.
func (r *spawnRecord) spawned(want versionroute.Quarter) []*fakeMCPClient {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*fakeMCPClient, len(r.clients[want]))
	copy(out, r.clients[want])
	return out
}

// allSpawned returns the flat list of every client ever spawned,
// across all quarters. Useful for "did Close get called on every
// child" assertions.
func (r *spawnRecord) allSpawned() []*fakeMCPClient {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*fakeMCPClient
	for _, cs := range r.clients {
		out = append(out, cs...)
	}
	return out
}

// asSpawnFunc returns a spawnFunc that, on each call, either fires
// a queued failNext error or hands out a fresh fakeMCPClient
// constructed by builder. builder lets tests pre-configure the
// fake's callErr/callDelay/closeErr without touching the spawn
// plumbing.
func (r *spawnRecord) asSpawnFunc(builder func(versionroute.Quarter) *fakeMCPClient) spawnFunc {
	return func(_ context.Context, want versionroute.Quarter, _ string, _ time.Duration) (mcpClient, error) {
		r.mu.Lock()
		if err, ok := r.errs[want]; ok {
			delete(r.errs, want)
			r.mu.Unlock()
			return nil, err
		}
		c := builder(want)
		r.clients[want] = append(r.clients[want], c)
		r.mu.Unlock()
		return c, nil
	}
}

// installFakeSibling drops an empty 0o755 file named for want's
// backend into a tempdir and prepends it to PATH so
// versionroute.FindBackend can locate it. Returns the path. The
// fake never actually runs — the pool's spawnFunc is replaced via
// withSpawn — but FindBackend's stat-and-PATH walk needs a real
// filesystem entry.
func installFakeSibling(t *testing.T, want versionroute.Quarter) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("PATH semantics + 0o755 perm bits differ on Windows; covered by integration test")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, want.BackendName())
	require.NoError(t, os.WriteFile(path, []byte{}, 0o755))
	t.Setenv("PATH", dir)
	return path
}

// q is a tiny helper for readability in test setup.
func q(year, quarter int) versionroute.Quarter {
	return versionroute.Quarter{Year: year, Q: quarter}
}

// dispatchOK is shorthand for "Dispatch must succeed and return a
// non-error result" — most tests don't care about the response
// shape, only that the call completed.
func dispatchOK(t *testing.T, p *PoolRouter, want versionroute.Quarter) {
	t.Helper()
	res, err := p.Dispatch(context.Background(), want, mcp.CallToolRequest{})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "unexpected tool-level error")
}

// TestPoolWarmReuse pins the central value proposition: two
// Dispatch calls for the same quarter share one warm child, so the
// second call skips spawn-and-init.
func TestPoolWarmReuse(t *testing.T) {
	want := q(26, 1)
	installFakeSibling(t, want)

	rec := newSpawnRecord()
	p := NewPoolRouter(
		WithIdleTimeout(0), // disable janitor — test is timing-free
		withSpawn(rec.asSpawnFunc(func(versionroute.Quarter) *fakeMCPClient {
			return &fakeMCPClient{}
		})),
	)
	t.Cleanup(func() { _ = p.Close() })

	dispatchOK(t, p, want)
	dispatchOK(t, p, want)

	clients := rec.spawned(want)
	require.Len(t, clients, 1, "second Dispatch must reuse the warm child")
	calls, _, _, _ := clients[0].snapshot()
	require.Equal(t, 2, calls, "both Dispatches should hit the same client")
}

// TestPoolPerQuarterIsolation pins that two distinct quarters get
// distinct children. A regression that keyed the pool incorrectly
// (e.g. by path or by backend name string) would surface here as
// the first quarter's child being reused for the second.
func TestPoolPerQuarterIsolation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH semantics differ on Windows")
	}
	q1 := q(26, 1)
	q2 := q(26, 2)
	dir := t.TempDir()
	for _, qq := range []versionroute.Quarter{q1, q2} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, qq.BackendName()), []byte{}, 0o755))
	}
	t.Setenv("PATH", dir)

	rec := newSpawnRecord()
	p := NewPoolRouter(
		WithIdleTimeout(0),
		withSpawn(rec.asSpawnFunc(func(versionroute.Quarter) *fakeMCPClient {
			return &fakeMCPClient{}
		})),
	)
	t.Cleanup(func() { _ = p.Close() })

	dispatchOK(t, p, q1)
	dispatchOK(t, p, q2)
	dispatchOK(t, p, q1)

	require.Len(t, rec.spawned(q1), 1)
	require.Len(t, rec.spawned(q2), 1)
}

// TestPoolIdleEviction pins that an entry whose lastUsed is older
// than idleTimeout is closed by the janitor and the next Dispatch
// re-spawns. Drives the clock manually so the test does not have
// to wait real time.
func TestPoolIdleEviction(t *testing.T) {
	want := q(26, 1)
	installFakeSibling(t, want)

	var nowNanos atomic.Int64
	start := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	nowNanos.Store(start.UnixNano())
	now := func() time.Time { return time.Unix(0, nowNanos.Load()) }

	rec := newSpawnRecord()
	p := NewPoolRouter(
		WithIdleTimeout(5*time.Minute),
		withJanitorTick(0), // disable background janitor; drive sweepIdle directly
		withSpawn(rec.asSpawnFunc(func(versionroute.Quarter) *fakeMCPClient {
			return &fakeMCPClient{}
		})),
		withClock(now),
	)
	t.Cleanup(func() { _ = p.Close() })

	dispatchOK(t, p, want)
	clients := rec.spawned(want)
	require.Len(t, clients, 1)

	// Advance 6 minutes (past idleTimeout) and sweep.
	nowNanos.Store(start.Add(6 * time.Minute).UnixNano())
	p.sweepIdle()

	_, _, closes, closed := clients[0].snapshot()
	require.Equal(t, 1, closes, "evicted child must be closed exactly once")
	require.True(t, closed)

	dispatchOK(t, p, want)
	require.Len(t, rec.spawned(want), 2, "post-eviction Dispatch must re-spawn")
}

// TestPoolDeadChildRecovery pins that a transport error from
// CallTool evicts the entry, closes the dead child, and that the
// next Dispatch re-spawns and succeeds. This is the broken-pipe /
// child-exit recovery path.
func TestPoolDeadChildRecovery(t *testing.T) {
	want := q(26, 1)
	path := installFakeSibling(t, want)

	var spawnIdx atomic.Int32
	rec := newSpawnRecord()
	p := NewPoolRouter(
		WithIdleTimeout(0),
		withSpawn(rec.asSpawnFunc(func(versionroute.Quarter) *fakeMCPClient {
			// First spawn returns a client whose CallTool fails;
			// second spawn returns a healthy client.
			if spawnIdx.Add(1) == 1 {
				return &fakeMCPClient{callErr: errors.New("broken pipe")}
			}
			return &fakeMCPClient{}
		})),
	)
	t.Cleanup(func() { _ = p.Close() })

	_, err := p.Dispatch(context.Background(), want, mcp.CallToolRequest{})
	require.ErrorContains(t, err, "broken pipe")
	require.ErrorContains(t, err, want.BackendName())
	require.ErrorContains(t, err, path,
		"transport error must include the resolved sibling path so an install/PATH problem is debuggable from one line")

	clients := rec.spawned(want)
	require.Len(t, clients, 1)
	_, _, closes, closed := clients[0].snapshot()
	require.Equal(t, 1, closes, "dead child must be closed exactly once on transport error")
	require.True(t, closed)

	dispatchOK(t, p, want)
	require.Len(t, rec.spawned(want), 2, "post-failure Dispatch must re-spawn transparently")
}

// TestPoolDeadChildRecoveryJoinsCloseError pins that when the
// transport-error path's eviction Close ALSO fails, the operator
// sees both errors joined into Dispatch's return value rather than
// a clean broken-pipe error here plus a swallowed close failure
// hidden in stderr. Without this, a sibling that hangs on
// shutdown after refusing a call would leak silently.
func TestPoolDeadChildRecoveryJoinsCloseError(t *testing.T) {
	want := q(26, 1)
	installFakeSibling(t, want)

	rec := newSpawnRecord()
	p := NewPoolRouter(
		WithIdleTimeout(0),
		withSpawn(rec.asSpawnFunc(func(versionroute.Quarter) *fakeMCPClient {
			return &fakeMCPClient{
				callErr:  errors.New("broken pipe"),
				closeErr: errors.New("sibling refused EOF"),
			}
		})),
	)
	t.Cleanup(func() { _ = p.Close() })

	_, err := p.Dispatch(context.Background(), want, mcp.CallToolRequest{})
	require.ErrorContains(t, err, "broken pipe",
		"transport error must surface")
	require.ErrorContains(t, err, "sibling refused EOF",
		"close-time error must be joined onto the transport error")
	require.ErrorContains(t, err, "close after transport error",
		"close failure must be labelled so the operator can tell which step failed")
}

// TestPoolSerializesSameQuarter pins the bounded-concurrency
// guarantee from issue #145: mark3labs/mcp-go's stdio Client is not
// safe for concurrent CallTool, so per-quarter requests must
// serialize through the entry's mutex. The fake records the
// observed concurrency level; if the pool ever lets two CallTools
// run on the same client simultaneously, maxInFlight will be > 1.
func TestPoolSerializesSameQuarter(t *testing.T) {
	want := q(26, 1)
	installFakeSibling(t, want)

	rec := newSpawnRecord()
	p := NewPoolRouter(
		WithIdleTimeout(0),
		withSpawn(rec.asSpawnFunc(func(versionroute.Quarter) *fakeMCPClient {
			return &fakeMCPClient{callDelay: 50 * time.Millisecond}
		})),
	)
	t.Cleanup(func() { _ = p.Close() })

	const goroutines = 5
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			dispatchOK(t, p, want)
		}()
	}
	wg.Wait()

	clients := rec.spawned(want)
	require.Len(t, clients, 1, "all goroutines must share one warm child")
	calls, maxInFlight, _, _ := clients[0].snapshot()
	require.Equal(t, goroutines, calls)
	require.Equal(t, 1, maxInFlight,
		"per-quarter CallTool must serialize; observed concurrency %d violates mcp-go's contract",
		maxInFlight)
}

// TestPoolGracefulShutdown pins that Close stops the janitor,
// closes every warm child exactly once, and that subsequent
// Dispatch returns a transport error rather than silently spawning
// a new child after shutdown.
func TestPoolGracefulShutdown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH semantics differ on Windows")
	}
	q1 := q(26, 1)
	q2 := q(26, 2)
	dir := t.TempDir()
	for _, qq := range []versionroute.Quarter{q1, q2} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, qq.BackendName()), []byte{}, 0o755))
	}
	t.Setenv("PATH", dir)

	rec := newSpawnRecord()
	p := NewPoolRouter(
		WithIdleTimeout(0),
		withSpawn(rec.asSpawnFunc(func(versionroute.Quarter) *fakeMCPClient {
			return &fakeMCPClient{}
		})),
	)

	dispatchOK(t, p, q1)
	dispatchOK(t, p, q2)

	require.NoError(t, p.Close())
	require.NoError(t, p.Close(), "second Close must be a no-op")

	for _, c := range rec.allSpawned() {
		_, _, closes, closed := c.snapshot()
		require.Equal(t, 1, closes, "every pooled child must be closed exactly once")
		require.True(t, closed)
	}

	_, err := p.Dispatch(context.Background(), q1, mcp.CallToolRequest{})
	require.ErrorContains(t, err, "closed",
		"Dispatch on a closed pool must surface a transport error")
}

// TestPoolGracefulShutdownReportsCloseErrors pins that Close
// surfaces close-time errors from the underlying clients, joined
// across all of them, so a misbehaving sibling is visible to the
// operator rather than silently swallowed.
func TestPoolGracefulShutdownReportsCloseErrors(t *testing.T) {
	want := q(26, 1)
	installFakeSibling(t, want)

	rec := newSpawnRecord()
	p := NewPoolRouter(
		WithIdleTimeout(0),
		withSpawn(rec.asSpawnFunc(func(versionroute.Quarter) *fakeMCPClient {
			return &fakeMCPClient{closeErr: errors.New("sibling refused EOF")}
		})),
	)

	dispatchOK(t, p, want)
	err := p.Close()
	require.ErrorContains(t, err, "sibling refused EOF")
	require.ErrorContains(t, err, want.BackendName())
}

// TestPoolMissingBackend pins that a Quarter with no installed
// sibling produces the missing-backend tool error (with the
// discovery hint) rather than a transport-layer Go error. This is
// the behavior the deleted SpawnRouter tests covered.
func TestPoolMissingBackend(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH semantics differ on Windows")
	}
	// Empty PATH so FindBackend has nowhere to look.
	t.Setenv("PATH", t.TempDir())

	rec := newSpawnRecord()
	p := NewPoolRouter(
		WithIdleTimeout(0),
		withSpawn(rec.asSpawnFunc(func(versionroute.Quarter) *fakeMCPClient {
			return &fakeMCPClient{}
		})),
	)
	t.Cleanup(func() { _ = p.Close() })

	res, err := p.Dispatch(context.Background(), fakeQuarter, mcp.CallToolRequest{})
	require.NoError(t, err, "missing-backend must be a tool error, not a transport error")
	text := requireToolError(t, res)
	require.Contains(t, text, fakeQuarter.BackendName())
	require.Contains(t, text, "not installed")
	require.Contains(t, text, "Install the "+fakeQuarter.Tag()+" backend")

	require.Empty(t, rec.allSpawned(),
		"missing-backend path must not call spawn; it's resolved before that")
}

// TestPoolMissingBackendListsAlternatives pins that the discovery
// list ("Available backends:") survives the proxy → pool path. The
// deleted SpawnRouter test covered this for the spawn-per-call
// router; the pool reuses missingBackendResult verbatim, so the
// shape is identical.
func TestPoolMissingBackendListsAlternatives(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH semantics differ on Windows")
	}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "crdb-sql-v261"), []byte{}, 0o755))
	t.Setenv("PATH", dir)

	p := NewPoolRouter(WithIdleTimeout(0), withSpawn(newSpawnRecord().asSpawnFunc(
		func(versionroute.Quarter) *fakeMCPClient { return &fakeMCPClient{} },
	)))
	t.Cleanup(func() { _ = p.Close() })

	res, err := p.Dispatch(context.Background(), fakeQuarter, mcp.CallToolRequest{})
	require.NoError(t, err)
	text := requireToolError(t, res)
	require.Contains(t, text, "Available backends:")
	require.Contains(t, text, "crdb-sql-v261")
	require.Contains(t, text, fakeQuarter.BackendName())
}

// TestPoolSpawnFailureDoesNotPoisonEntry pins that a failed
// spawn-and-init is recoverable: the entry is dropped so the next
// Dispatch starts from scratch rather than repeatedly hitting the
// same poisoned slot.
func TestPoolSpawnFailureDoesNotPoisonEntry(t *testing.T) {
	want := q(26, 1)
	installFakeSibling(t, want)

	rec := newSpawnRecord()
	rec.failNext(want, errors.New("init failed"))
	p := NewPoolRouter(
		WithIdleTimeout(0),
		withSpawn(rec.asSpawnFunc(func(versionroute.Quarter) *fakeMCPClient {
			return &fakeMCPClient{}
		})),
	)
	t.Cleanup(func() { _ = p.Close() })

	_, err := p.Dispatch(context.Background(), want, mcp.CallToolRequest{})
	require.ErrorContains(t, err, "init failed")

	dispatchOK(t, p, want)
	require.Len(t, rec.spawned(want), 1,
		"second spawn (after the first failed before recording) is the only successful one")
}

// TestPoolJanitorRespectsActiveCalls pins that a janitor sweep does
// not close a child while a CallTool is in flight: the eviction
// blocks on the entry's mutex until the call completes. The test
// uses an explicit barrier inside the fake's CallTool so the
// "sweep was forced to wait" property is observable rather than
// inferred from timing — a sleep-based version would pass even if
// the sweep raced and closed the child immediately.
func TestPoolJanitorRespectsActiveCalls(t *testing.T) {
	want := q(26, 1)
	installFakeSibling(t, want)

	var nowNanos atomic.Int64
	start := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	nowNanos.Store(start.UnixNano())
	now := func() time.Time { return time.Unix(0, nowNanos.Load()) }

	entered := make(chan struct{})
	release := make(chan struct{})
	rec := newSpawnRecord()
	p := NewPoolRouter(
		WithIdleTimeout(1*time.Second),
		withJanitorTick(0),
		withSpawn(rec.asSpawnFunc(func(versionroute.Quarter) *fakeMCPClient {
			return &fakeMCPClient{callEntered: entered, callRelease: release}
		})),
		withClock(now),
	)
	t.Cleanup(func() { _ = p.Close() })

	dispatchDone := make(chan error, 1)
	go func() {
		_, err := p.Dispatch(context.Background(), want, mcp.CallToolRequest{})
		dispatchDone <- err
	}()

	// Block until CallTool has entered — Dispatch now holds the
	// entry's mutex.
	<-entered

	// Run sweep in a goroutine so we can prove it blocks. Advance
	// the clock past idleTimeout first.
	nowNanos.Store(start.Add(2 * time.Second).UnixNano())
	sweepDone := make(chan struct{})
	go func() {
		p.sweepIdle()
		close(sweepDone)
	}()

	// Verify the sweep does NOT complete while CallTool is in
	// flight. 50ms is comfortably more than the runtime would need
	// to schedule the sweep goroutine; if the sweep had not
	// blocked, sweepDone would be closed by now.
	select {
	case <-sweepDone:
		t.Fatal("sweepIdle returned while CallTool was still holding the entry mutex — eviction did not wait")
	case <-time.After(50 * time.Millisecond):
	}

	// Release the in-flight CallTool. Sweep should now be able to
	// take the entry's mutex and proceed.
	close(release)

	require.NoError(t, <-dispatchDone, "in-flight CallTool must complete cleanly even when janitor races to evict")
	select {
	case <-sweepDone:
	case <-time.After(2 * time.Second):
		t.Fatal("sweepIdle did not return after CallTool released the entry mutex")
	}

	clients := rec.spawned(want)
	require.Len(t, clients, 1)
	_, _, closes, _ := clients[0].snapshot()
	require.Equal(t, 1, closes, "evicted child must be closed exactly once")
}

// TestPoolJanitorDoesNotEvictWarmEntries is the negative companion
// to TestPoolIdleEviction: an entry that has been used recently
// must survive the sweep.
func TestPoolJanitorDoesNotEvictWarmEntries(t *testing.T) {
	want := q(26, 1)
	installFakeSibling(t, want)

	var nowNanos atomic.Int64
	start := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	nowNanos.Store(start.UnixNano())
	now := func() time.Time { return time.Unix(0, nowNanos.Load()) }

	rec := newSpawnRecord()
	p := NewPoolRouter(
		WithIdleTimeout(5*time.Minute),
		withJanitorTick(0),
		withSpawn(rec.asSpawnFunc(func(versionroute.Quarter) *fakeMCPClient {
			return &fakeMCPClient{}
		})),
		withClock(now),
	)
	t.Cleanup(func() { _ = p.Close() })

	dispatchOK(t, p, want)
	// Advance 1 minute (well under idleTimeout) and sweep.
	nowNanos.Store(start.Add(1 * time.Minute).UnixNano())
	p.sweepIdle()

	clients := rec.spawned(want)
	require.Len(t, clients, 1)
	_, _, closes, _ := clients[0].snapshot()
	require.Equal(t, 0, closes, "warm child must survive a sweep before idleTimeout elapses")

	dispatchOK(t, p, want)
	require.Len(t, rec.spawned(want), 1, "second Dispatch must reuse the surviving child")
}

// TestPoolDispatchRetriesAfterEvictionRace pins the C1 race fix:
// when an entry is evicted between checkout returning it and
// Dispatch acquiring its mutex, Dispatch must restart with a
// fresh checkout rather than re-spawn into the dead entry. The
// dead-entry path would silently leak the new client because the
// entry is no longer in PoolRouter.entries — no one will close it.
//
// Uses the dispatchAfterCheckout test hook to force the race
// deterministically: the hook fires after checkout returns the
// warm entry but before Dispatch can lock it, evicting it under
// pool.mu so the subsequent Lock observes dead=true.
func TestPoolDispatchRetriesAfterEvictionRace(t *testing.T) {
	want := q(26, 1)
	installFakeSibling(t, want)

	rec := newSpawnRecord()
	// hookArmed gates which Dispatch triggers the eviction. The
	// warm-up call must run normally; the second call exercises
	// the retry path. Atomic load+store rather than a bool because
	// the hook may run from a different goroutine in future
	// extensions.
	var hookArmed atomic.Bool
	var p *PoolRouter
	hook := func(w versionroute.Quarter, entry *pooledEntry) {
		if !hookArmed.CompareAndSwap(true, false) {
			return
		}
		// Snapshot the warm client under the entry's mutex (the
		// hook runs before Dispatch's own Lock, so we can take
		// it freely), then evict-and-close — mirrors the natural
		// race shape where sweepIdle would do the same thing.
		entry.Lock()
		c := entry.client
		entry.client = nil
		entry.Unlock()
		require.NoError(t, p.evict(w, entry, c),
			"hook's evict must succeed; the production retry path depends on a clean eviction here")
	}
	p = NewPoolRouter(
		WithIdleTimeout(0),
		withSpawn(rec.asSpawnFunc(func(versionroute.Quarter) *fakeMCPClient {
			return &fakeMCPClient{}
		})),
		withDispatchAfterCheckout(hook),
	)
	t.Cleanup(func() { _ = p.Close() })

	// Warm-up: hook is unarmed so this Dispatch completes the
	// straight-through path and leaves a live entry in the pool.
	dispatchOK(t, p, want)
	require.Len(t, rec.spawned(want), 1, "warm-up Dispatch must spawn exactly once")

	// Arm the hook so the next Dispatch's checkout-to-Lock window
	// closes with the entry already evicted.
	hookArmed.Store(true)
	dispatchOK(t, p, want)
	require.False(t, hookArmed.Load(), "hook must have consumed its arm signal")
	require.Len(t, rec.spawned(want), 2,
		"Dispatch must re-spawn after the dead-entry retry (one warm-up + one retry)")

	clients := rec.spawned(want)
	_, _, closes0, _ := clients[0].snapshot()
	require.Equal(t, 1, closes0, "evicted warm child must have been closed by the hook's evict")
	_, _, closes1, _ := clients[1].snapshot()
	require.Equal(t, 0, closes1, "post-retry client is still warm in the pool")
}

// TestPoolWithIdleTimeoutZeroSkipsJanitor pins that
// WithIdleTimeout(0) opts out of background eviction entirely:
// no janitor goroutine, and an entry that has been "idle" for an
// arbitrary duration survives a manual sweepIdle. The behavioral
// half is the load-bearing assertion; the janitor-not-running half
// is verified by Close completing without any goroutine wait
// (other tests' t.Cleanup calls would hang otherwise).
func TestPoolWithIdleTimeoutZeroSkipsJanitor(t *testing.T) {
	want := q(26, 1)
	installFakeSibling(t, want)

	var nowNanos atomic.Int64
	start := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	nowNanos.Store(start.UnixNano())
	now := func() time.Time { return time.Unix(0, nowNanos.Load()) }

	rec := newSpawnRecord()
	p := NewPoolRouter(
		WithIdleTimeout(0),
		withSpawn(rec.asSpawnFunc(func(versionroute.Quarter) *fakeMCPClient {
			return &fakeMCPClient{}
		})),
		withClock(now),
	)
	t.Cleanup(func() { _ = p.Close() })

	dispatchOK(t, p, want)
	// Advance an unreasonably long time and force a sweep.
	// Behavior with idleTimeout=0: nothing is ever idle enough.
	nowNanos.Store(start.Add(48 * time.Hour).UnixNano())
	p.sweepIdle()

	clients := rec.spawned(want)
	require.Len(t, clients, 1)
	_, _, closes, _ := clients[0].snapshot()
	require.Equal(t, 0, closes,
		"WithIdleTimeout(0) must keep the warm child alive indefinitely")

	dispatchOK(t, p, want)
	require.Len(t, rec.spawned(want), 1, "subsequent Dispatch must reuse the same warm child")
}

// TestPoolCloseWaitsForActiveDispatch pins that Close blocks on
// in-flight CallTool rather than yanking the pipe out from under
// it. Uses the same barrier pattern as
// TestPoolJanitorRespectsActiveCalls so the wait property is
// observable, not inferred from timing.
func TestPoolCloseWaitsForActiveDispatch(t *testing.T) {
	want := q(26, 1)
	installFakeSibling(t, want)

	entered := make(chan struct{})
	release := make(chan struct{})
	rec := newSpawnRecord()
	p := NewPoolRouter(
		WithIdleTimeout(0),
		withSpawn(rec.asSpawnFunc(func(versionroute.Quarter) *fakeMCPClient {
			return &fakeMCPClient{callEntered: entered, callRelease: release}
		})),
	)

	dispatchDone := make(chan error, 1)
	go func() {
		_, err := p.Dispatch(context.Background(), want, mcp.CallToolRequest{})
		dispatchDone <- err
	}()
	<-entered

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- p.Close()
	}()

	// Close must NOT complete while CallTool is still in flight —
	// it should be blocked on the entry's mutex.
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned (err=%v) while CallTool was still holding the entry mutex", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)

	require.NoError(t, <-dispatchDone, "in-flight CallTool must complete cleanly during Close drain")
	select {
	case err := <-closeDone:
		require.NoError(t, err, "graceful Close after drain must succeed")
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after CallTool released the entry mutex")
	}

	clients := rec.spawned(want)
	require.Len(t, clients, 1)
	_, _, closes, _ := clients[0].snapshot()
	require.Equal(t, 1, closes, "warm child must be closed exactly once during graceful drain")

	// Post-Close Dispatch must surface the closed-pool error.
	_, err := p.Dispatch(context.Background(), want, mcp.CallToolRequest{})
	require.ErrorContains(t, err, "closed")
}
