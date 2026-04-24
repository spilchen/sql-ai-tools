// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/spilchen/sql-ai-tools/internal/versionroute"
)

// Production defaults. Pulled out as named constants so the docs in
// PoolRouter, the help text in cmd/mcp.go, and the tests all
// reference one source of truth.
const (
	// defaultIdleTimeout is the upper bound on how long a warm
	// sibling sits unused before the janitor closes it. 5 minutes is
	// long enough to amortize spawn cost across a typical Claude
	// Code session and short enough that switching to a different
	// quarter does not pin the previous sibling's RAM for an hour.
	defaultIdleTimeout = 5 * time.Minute

	// defaultJanitorTick controls how often the janitor sweeps for
	// idle entries. Coarse relative to defaultIdleTimeout because
	// being a minute late on eviction is harmless; the cost of a
	// shorter tick is goroutine wakeups for nothing.
	defaultJanitorTick = 1 * time.Minute
)

// mcpClient is the slice of mark3labs/mcp-go's *client.Client that
// the pool actually uses. Pulling it behind an interface lets
// pool_test.go drive deterministic scenarios (transport error on
// CallTool, slow CallTool, Close error) without spawning real
// processes. The production spawn function (defaultSpawnFunc) wraps
// *client.Client to satisfy this interface.
type mcpClient interface {
	CallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)
	Close() error
}

// spawnFunc launches a sibling at path, runs the MCP initialize
// handshake under initTimeout, and returns the connected client.
// want is used only to compose error messages. The pool's
// production constructor wires defaultSpawnFunc; tests inject a
// fake.
type spawnFunc func(
	ctx context.Context, want versionroute.Quarter, path string, initTimeout time.Duration,
) (mcpClient, error)

// defaultSpawnFunc is the production spawnFunc: thin shim around
// spawnAndInit so tests can swap the whole spawn path.
func defaultSpawnFunc(
	ctx context.Context, want versionroute.Quarter, path string, initTimeout time.Duration,
) (mcpClient, error) {
	c, err := spawnAndInit(ctx, want, path, initTimeout)
	if err != nil {
		return nil, err
	}
	return clientAdapter{c}, nil
}

// clientAdapter narrows *client.Client (which has many MCP-protocol
// methods) to the mcpClient interface the pool depends on.
type clientAdapter struct{ c *client.Client }

func (a clientAdapter) CallTool(
	ctx context.Context, req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	return a.c.CallTool(ctx, req)
}

func (a clientAdapter) Close() error { return a.c.Close() }

// PoolRouter implements Router by keeping at most one warm sibling
// child per Quarter for the life of the parent `crdb-sql mcp`
// process. The first Dispatch for a Quarter spawns the child and
// runs the MCP initialize handshake; subsequent Dispatch calls reuse
// the warm child, amortizing spawn + handshake cost across calls.
//
// Lifecycle:
//   - Lazy spawn: a Quarter's child is only started on the first
//     Dispatch for that Quarter.
//   - Bounded concurrency per Quarter: one child, serialized by the
//     entry's mutex. mark3labs/mcp-go's stdio Client is not safe
//     for concurrent CallTool, so per-quarter requests queue.
//   - Idle eviction: the janitor goroutine sweeps every
//     janitorTick; entries unused for longer than idleTimeout are
//     closed. The next Dispatch re-spawns transparently.
//   - Dead-child recovery: if CallTool returns a transport-layer
//     error, the entry is evicted and closed before Dispatch
//     returns. The next Dispatch re-spawns transparently.
//   - Graceful shutdown: Close stops the janitor and closes every
//     pooled child. cmd/mcp.go defers it after server.ServeStdio
//     returns so a clean parent exit propagates to all children.
//
// The error contract is the same as the package-level Router doc:
// transport failures (spawn, init, broken pipe) propagate as Go
// errors; missing-sibling and tool-level failures come back as
// IsError=true *mcp.CallToolResult with a nil error.
type PoolRouter struct {
	// initTimeout caps the MCP initialize handshake on each spawned
	// child. Per-tool-call timeouts flow from the ctx the caller
	// supplies to Dispatch.
	initTimeout time.Duration

	// idleTimeout is the upper bound on how long a warm child sits
	// unused before the janitor closes it.
	idleTimeout time.Duration

	// janitorTick is the period of the eviction sweep. Coarser than
	// idleTimeout — eviction is best-effort, not a hard deadline.
	janitorTick time.Duration

	// spawn launches and initializes a sibling. Replaceable for
	// tests; production wiring uses defaultSpawnFunc.
	spawn spawnFunc

	// nowFn returns the current time. Replaceable for tests so
	// idle-eviction can be exercised without sleeping.
	nowFn func() time.Time

	// dispatchAfterCheckout, when non-nil, is called inside
	// Dispatch after checkout returns and before the entry's mutex
	// is acquired. Test-only hook (set via withDispatchAfterCheckout)
	// for deterministically forcing the eviction-race retry path —
	// without it, that path is timing-dependent and impossible to
	// exercise without sleeping. nil in production.
	dispatchAfterCheckout func(versionroute.Quarter, *pooledEntry)

	// mu protects entries, closed, and each entry's lastUsed/dead
	// fields. It does NOT protect entry.client or the entry's
	// embedded Mutex — those are owned by the entry itself so a
	// long-running CallTool against one quarter does not block
	// lookups for another.
	//
	// Lock ordering: when both mu and an entry's mutex are needed,
	// mu is taken first OR released before the entry's mutex is
	// acquired. The reverse order — holding an entry's mutex and
	// then taking mu — is permitted only for the brief, leaf-level
	// updates in markUsed and evict, where mu is released before
	// any further work. Sweep and Close always release mu before
	// taking an entry's mutex; that asymmetry, plus the
	// release-before-work rule, prevents a cycle.
	mu      sync.Mutex
	entries map[versionroute.Quarter]*pooledEntry
	closed  bool

	// stopJanitor cancels the janitor goroutine's context. Set in
	// NewPoolRouter; nil after Close.
	stopJanitor context.CancelFunc

	// janitorDone is closed when the janitor goroutine exits, so
	// Close can wait for it before returning. Nil if the janitor
	// was never started (idleTimeout <= 0).
	janitorDone chan struct{}
}

// pooledEntry holds one warm sibling child for one Quarter.
//
// Lifecycle: created by Dispatch on first call for the quarter
// (with client = nil), initialized under the entry's mutex by the
// same Dispatch (or a later one if init failed), reused by
// subsequent Dispatch calls under the same mutex, evicted by the
// janitor after idleTimeout or on the spot when CallTool returns a
// transport error. After eviction the entry is dropped from
// PoolRouter.entries and the next Dispatch for the quarter creates
// a fresh entry.
//
// Embedded sync.Mutex serializes both lazy initialization and
// CallTool against client. mark3labs/mcp-go's stdio Client is not
// concurrency-safe for CallTool, so per-quarter requests queue
// here.
//
// quarter and path are immutable after construction (set in
// PoolRouter.checkout, never written again).
type pooledEntry struct {
	sync.Mutex
	quarter versionroute.Quarter
	path    string

	// client is nil until the first successful spawn-and-init,
	// reset to nil by the eviction or Close paths after the
	// underlying client is closed. Read and written under the
	// embedded Mutex.
	client mcpClient

	// lastUsed tracks the wall-clock time of the last successful
	// Dispatch interaction with this entry. Written by checkout
	// (when reusing a warm entry) and by markUsed (after a
	// successful CallTool). Transport errors do not update
	// lastUsed — they evict the entry instead. Read by the janitor
	// under PoolRouter.mu, so it can scan idleness without blocking
	// on an in-flight CallTool.
	lastUsed time.Time

	// dead, when true, marks the entry as evicted from
	// PoolRouter.entries; a Dispatch goroutine that won the entry's
	// mutex on a stale pointer must retry checkout rather than
	// re-spawning into a slot no future caller can find. Always
	// read and written under PoolRouter.mu (evict, sweepIdle, and
	// Close all set it under that lock).
	dead bool
}

// PoolOption configures NewPoolRouter. Matches the project's
// functional-options convention (.claude/rules/go-conventions.md).
type PoolOption interface {
	apply(*PoolRouter)
}

type poolOptionFunc func(*PoolRouter)

func (f poolOptionFunc) apply(p *PoolRouter) { f(p) }

// WithIdleTimeout overrides the default idle-eviction window. A
// non-positive value disables idle eviction entirely (warm children
// live until Close).
func WithIdleTimeout(d time.Duration) PoolOption {
	return poolOptionFunc(func(p *PoolRouter) { p.idleTimeout = d })
}

// WithInitTimeout overrides the default MCP initialize handshake
// budget. Must be positive.
func WithInitTimeout(d time.Duration) PoolOption {
	return poolOptionFunc(func(p *PoolRouter) { p.initTimeout = d })
}

// withJanitorTick overrides the janitor sweep interval. Test-only;
// production callers want the default. Exported lowercase so the
// test file in the same package can use it.
func withJanitorTick(d time.Duration) PoolOption {
	return poolOptionFunc(func(p *PoolRouter) { p.janitorTick = d })
}

// withSpawn replaces the spawn function. Test-only.
func withSpawn(s spawnFunc) PoolOption {
	return poolOptionFunc(func(p *PoolRouter) { p.spawn = s })
}

// withClock replaces the time source. Test-only.
func withClock(now func() time.Time) PoolOption {
	return poolOptionFunc(func(p *PoolRouter) { p.nowFn = now })
}

// withDispatchAfterCheckout installs a hook that runs in Dispatch
// after checkout returns and before the entry's mutex is acquired.
// Test-only — lets a test deterministically evict an entry inside
// that window to exercise Dispatch's dead-entry retry loop.
func withDispatchAfterCheckout(fn func(versionroute.Quarter, *pooledEntry)) PoolOption {
	return poolOptionFunc(func(p *PoolRouter) { p.dispatchAfterCheckout = fn })
}

// NewPoolRouter returns a pool with production defaults: 10s init
// timeout, 5m idle window, 1m janitor tick. The janitor goroutine
// starts immediately and stops on Close. Pass options to override
// any default.
func NewPoolRouter(opts ...PoolOption) *PoolRouter {
	p := &PoolRouter{
		initTimeout: defaultInitTimeout,
		idleTimeout: defaultIdleTimeout,
		janitorTick: defaultJanitorTick,
		spawn:       defaultSpawnFunc,
		nowFn:       time.Now,
		entries:     map[versionroute.Quarter]*pooledEntry{},
	}
	for _, opt := range opts {
		opt.apply(p)
	}
	if p.idleTimeout > 0 && p.janitorTick > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		p.stopJanitor = cancel
		p.janitorDone = make(chan struct{})
		go p.runJanitor(ctx)
	}
	return p
}

// Dispatch implements Router. See PoolRouter's doc for the
// lifecycle and the package-level Router doc for the error
// contract. After Close, every Dispatch returns a transport error
// naming the closed pool — not a tool-error result, because the
// caller's per-call timeout is irrelevant at that point and the
// shape matches "the connection is gone".
func (p *PoolRouter) Dispatch(
	ctx context.Context, want versionroute.Quarter, req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	// Retry loop handles the narrow window where a goroutine has
	// checked out an entry but, before acquiring the entry's mutex,
	// the janitor or a sibling Dispatch evicted it. Without the
	// retry, a re-spawn here would write a brand-new client into a
	// dead entry that no future caller can find — leaking the
	// child. The bound is the count of consecutive evictions we
	// race with, which is in practice 1 or 2.
	for attempt := 0; ; attempt++ {
		entry, err := p.checkout(want)
		if err != nil {
			return nil, err
		}
		if entry == nil {
			// FindBackend reported the sibling is not installed.
			// Tool-level error, no entry created.
			return missingBackendResult(want), nil
		}

		// Test-only hook: lets the C1 retry-path test force an
		// eviction inside the checkout-to-Lock window so the
		// `if entry.dead` branch below is observably reached.
		// nil in production.
		if hook := p.dispatchAfterCheckout; hook != nil {
			hook(want, entry)
		}

		// Take the entry's mutex before touching client so concurrent
		// Dispatch calls for the same quarter serialize. Any goroutine
		// that wins the lock first does the lazy init; the rest see a
		// non-nil client and skip straight to CallTool.
		entry.Lock()
		if entry.dead {
			// Lost the race with eviction. Release the mutex
			// (under which dead is now safe to re-read alongside
			// pool.mu's writers) and start over with a fresh
			// checkout.
			entry.Unlock()
			if attempt > 16 {
				// Defensive: the retry loop should converge in
				// 1-2 attempts in any realistic scenario.
				// Surface a transport error rather than spin.
				return nil, fmt.Errorf("dispatch %s: pool churn — gave up after %d eviction races",
					want.BackendName(), attempt)
			}
			continue
		}
		return p.dispatchLocked(ctx, want, req, entry)
	}
}

// dispatchLocked runs the spawn-or-reuse + CallTool path with
// entry's mutex held. It is responsible for releasing the mutex
// before any slow Close on the transport-error path so the caller
// is not pinned by mark3labs/mcp-go's shutdown grace (~8s worst
// case). Returns the same (result, error) shape as Dispatch.
func (p *PoolRouter) dispatchLocked(
	ctx context.Context, want versionroute.Quarter, req mcp.CallToolRequest, entry *pooledEntry,
) (*mcp.CallToolResult, error) {
	if entry.client == nil {
		c, err := p.spawn(ctx, want, entry.path, p.initTimeout)
		if err != nil {
			// Init failed — drop the empty entry so the next
			// Dispatch can retry with a fresh one rather than
			// re-using a poisoned slot. There is no client to
			// close yet (spawnAndInit tears down on failure).
			entry.Unlock()
			_ = p.evict(want, entry, nil /* clientToClose */)
			return nil, err
		}
		entry.client = c
	}

	res, callErr := entry.client.CallTool(ctx, req)
	if callErr != nil {
		// Transport-layer failure (broken pipe, framing error,
		// child exited): the connection is unusable. Capture the
		// client locally, nil it out under the mutex, then release
		// the mutex BEFORE closing — mcp-go's Client.Close can
		// take several seconds (stdin EOF + SIGTERM + SIGKILL with
		// grace windows), and holding the mutex through that would
		// pin every queued Dispatch on this Quarter for the same
		// duration even though they all need a fresh entry.
		dead := entry.client
		entry.client = nil
		entry.Unlock()
		closeErr := p.evict(want, entry, dead)
		wrapped := fmt.Errorf("forward tools/call to %s (%s): %w",
			want.BackendName(), entry.path, callErr)
		if closeErr != nil {
			// Surface the close-time failure alongside the
			// transport error so the operator sees both signals
			// in one message rather than a clean error here plus
			// a stderr line nobody reads.
			return nil, errors.Join(wrapped, fmt.Errorf("close after transport error: %w", closeErr))
		}
		return nil, wrapped
	}

	entry.Unlock()
	p.markUsed(entry)
	return res, nil
}

// checkout returns the pooled entry for want, creating it lazily.
// Returns (nil, nil) when the sibling is not installed (caller
// must surface a missing-backend result). Returns (nil, err) when
// the pool is closed.
func (p *PoolRouter) checkout(want versionroute.Quarter) (*pooledEntry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("crdb-sql mcp pool: closed")
	}
	if entry, ok := p.entries[want]; ok && !entry.dead {
		entry.lastUsed = p.nowFn()
		return entry, nil
	}
	path, found := versionroute.FindBackend(want)
	if !found {
		return nil, nil
	}
	entry := &pooledEntry{
		quarter:  want,
		path:     path,
		lastUsed: p.nowFn(),
	}
	p.entries[want] = entry
	return entry, nil
}

// markUsed updates an entry's lastUsed timestamp so the janitor
// resets its idle clock. Skips dead entries — a successful
// CallTool can race with eviction; updating lastUsed on an entry
// that no future caller can find would silently mutate dead state
// and make debugging confusing.
func (p *PoolRouter) markUsed(entry *pooledEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry.dead {
		return
	}
	entry.lastUsed = p.nowFn()
}

// evict removes entry from the pool's map and, if clientToClose is
// non-nil, closes it. The close error is returned so the caller
// (Dispatch's transport-error path) can join it onto the user-
// visible error rather than only logging to stderr. The janitor
// path (sweepIdle) does not call evict; it does its own
// remove-then-close because its caller has nowhere to surface
// errors except stderr.
func (p *PoolRouter) evict(
	want versionroute.Quarter, entry *pooledEntry, clientToClose mcpClient,
) error {
	p.mu.Lock()
	if cur, ok := p.entries[want]; ok && cur == entry {
		delete(p.entries, want)
	}
	entry.dead = true
	p.mu.Unlock()
	if clientToClose == nil {
		return nil
	}
	if err := clientToClose.Close(); err != nil {
		return fmt.Errorf("close evicted sibling %s (%s): %w",
			want.BackendName(), entry.path, err)
	}
	return nil
}

// runJanitor sweeps for idle entries every janitorTick and closes
// any whose lastUsed is older than idleTimeout. Exits when ctx is
// cancelled (Close). The sweep removes idle entries from
// PoolRouter.entries under pool.mu first (so no future Dispatch
// can find them), then takes each victim's mutex outside pool.mu
// to wait for any in-flight CallTool to finish before closing the
// underlying pipe. Worst-case wait per victim is one in-flight
// CallTool's remaining ctx deadline plus mcp-go's Close grace
// (~8s — see Close's doc).
func (p *PoolRouter) runJanitor(ctx context.Context) {
	defer close(p.janitorDone)
	t := time.NewTicker(p.janitorTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.sweepIdle()
		}
	}
}

// sweepIdle is the body of one janitor tick. Pulled out so tests
// can drive an eviction without depending on real-time tickers.
// A non-positive idleTimeout disables eviction entirely — the
// production NewPoolRouter never starts the janitor in that case,
// but a test that calls sweepIdle directly must still be a no-op
// so WithIdleTimeout(0) means "warm forever."
func (p *PoolRouter) sweepIdle() {
	if p.idleTimeout <= 0 {
		return
	}
	now := p.nowFn()
	p.mu.Lock()
	type victim struct {
		want  versionroute.Quarter
		entry *pooledEntry
	}
	var victims []victim
	for want, entry := range p.entries {
		if now.Sub(entry.lastUsed) >= p.idleTimeout {
			entry.dead = true
			delete(p.entries, want)
			victims = append(victims, victim{want, entry})
		}
	}
	p.mu.Unlock()

	// Close victims outside pool.mu so a slow Close (child not
	// exiting promptly) does not block lookups for other quarters.
	// Each victim's mutex must be taken to wait for any in-flight
	// CallTool to finish before closing the underlying pipe;
	// otherwise we'd close stdout out from under a goroutine
	// reading the response. Stderr is the only sink available — the
	// janitor has no caller to surface errors to.
	for _, v := range victims {
		v.entry.Lock()
		c := v.entry.client
		v.entry.client = nil
		v.entry.Unlock()
		if c == nil {
			continue
		}
		if err := c.Close(); err != nil {
			fmt.Fprintf(os.Stderr,
				"crdb-sql mcp pool: close idle sibling %s (%s): %v\n",
				v.want.BackendName(), v.entry.path, err)
		}
	}
}

// Close stops the janitor and closes every pooled child. The
// first call returns the joined Close errors from any pooled
// children (nil when every child closed cleanly); subsequent calls
// are no-ops that return nil. After Close, every Dispatch returns
// a transport-level "pool: closed" error.
//
// Close holds each entry's mutex briefly to wait for any in-flight
// CallTool. A misbehaving sibling that ignores stdin EOF is killed
// by mark3labs/mcp-go's Client.Close, which escalates stdin EOF →
// SIGTERM (after ~2s) → SIGKILL (after a further ~3s) with another
// ~3s grace, so a single hung child can stall Close for up to ~8s.
// We do not add a second timer on top of that.
func (p *PoolRouter) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	stop := p.stopJanitor
	done := p.janitorDone
	entries := p.entries
	p.entries = nil
	// Mark every entry dead while still under pool.mu. A Dispatch
	// goroutine that finished CallTool concurrently with Close may
	// be waiting to enter markUsed (which takes pool.mu); doing
	// the dead-write here ensures markUsed observes dead==true
	// under the same lock and skips its update — which is the
	// race the detector catches if the dead-write moves outside
	// pool.mu.
	for _, entry := range entries {
		entry.dead = true
	}
	p.mu.Unlock()

	if stop != nil {
		stop()
	}
	if done != nil {
		<-done
	}

	var errs []error
	for want, entry := range entries {
		entry.Lock()
		c := entry.client
		entry.client = nil
		entry.Unlock()
		if c == nil {
			continue
		}
		if err := c.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close %s (%s): %w",
				want.BackendName(), entry.path, err))
		}
	}
	return errors.Join(errs...)
}
