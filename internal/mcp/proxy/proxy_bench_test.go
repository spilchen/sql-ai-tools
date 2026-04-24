// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

//go:build integration

// Benchmark for issue #146: quantify the per-routed-call latency
// cost of three target_version routing strategies. The numbers feed
// back into PoolRouter's idle-eviction window and per-quarter slot
// count, and let future readers decide whether the spawn-per-call
// path that landed in #129 (and was replaced by PoolRouter in #145)
// is worth resurrecting for any workload.
//
//   - BenchmarkRouteSpawnPerCall — spawn + MCP init + call + close
//     per iteration. Reconstructs the #129 SpawnRouter contract on
//     top of the surviving spawnAndInit helper. Each iteration pays
//     full process-spawn plus initialize-handshake cost.
//   - BenchmarkRoutePooledWarm   — one PoolRouter, one warm-up
//     Dispatch outside the timer to spawn the child, then b.N
//     dispatches against the warm child. Steady-state cost,
//     dominated by the JSON-RPC round-trip.
//   - BenchmarkRoutePooledCold   — fresh PoolRouter per iteration
//     (NewPoolRouter → Dispatch → Close). Forces re-spawn every
//     time, so the per-op cost should track BenchmarkRouteSpawnPerCall.
//     Confirms the pool's only saving is reuse.
//
// Build is integration-tagged because it spawns real `crdb-sql` and
// `crdb-sql-v261` children, mirroring
// internal/versionroute/cross_version_integration_test.go and
// internal/mcp/cross_version_mcp_integration_test.go. Do NOT gate
// CI on these numbers — small-machine runners are too noisy. The
// expected invocation, also documented in
// internal/mcp/proxy/README.md, is:
//
//	go test -tags integration -run '^$' \
//	    -bench=BenchmarkRoute -benchtime=5x ./internal/mcp/proxy

package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/versionroute"
)

// benchSiblingQuarter is the parser quarter every benchmark routes
// to. Pinned to v26.1 because the worktree's build/go.v261.mod is
// the only sibling Makefile QUARTERS knows how to build.
var benchSiblingQuarter = versionroute.Quarter{Year: 26, Q: 1}

// benchSiblingParserVersion is the parser_version stamp the v26.1
// sibling places on every envelope. Used by requireSiblingResult to
// prove the warm-up call actually crossed the proxy boundary; a
// mismatch means the call was served by something other than the
// v26.1 sibling (a misconfigured PATH, a missing build, or a
// regression in PoolRouter that bypasses the sibling entirely).
const benchSiblingParserVersion = "v0.26.1"

// benchSetupErr, when non-nil, holds the error from TestMain's
// build/PATH setup. Each Benchmark consults it via b.Fatalf so a
// build failure surfaces as a benchmark failure with full context
// rather than a confusing "missing backend" tool-error result.
var benchSetupErr error

// TestMain builds crdb-sql (latest, stamped v262) and crdb-sql-v261
// once into a process-lifetime tempdir and prepends that dir to PATH
// so versionroute.FindBackend(v261) picks up the sibling on first
// Dispatch. Build failures stash into benchSetupErr rather than
// killing the process, so unrelated tests in this package (driven
// by `go test -tags integration ./...`) still get a chance to run.
func TestMain(m *testing.M) {
	if err := setupBenchBinaries(); err != nil {
		benchSetupErr = err
	}
	os.Exit(m.Run())
}

// setupBenchBinaries compiles both binaries into a tempdir and
// rewrites PATH. Mirrors the build/PATH recipe from
// internal/versionroute/cross_version_integration_test.go and
// internal/mcp/cross_version_mcp_integration_test.go; duplicated
// rather than shared because a cross-package test helper would need
// an internal/testutil shim that does not yet exist for any other
// caller.
//
// The tempdir leaks intentionally: TestMain has no testing.T to hang
// a Cleanup on, and two extra binaries in /tmp until the developer's
// next reboot is an acceptable cost for an integration-tagged bench.
func setupBenchBinaries() error {
	if _, err := exec.LookPath("go"); err != nil {
		return fmt.Errorf("go toolchain not on PATH: %w", err)
	}
	repoRoot, err := findRepoRootBench()
	if err != nil {
		return err
	}
	binDir, err := os.MkdirTemp("", "crdb-sql-proxy-bench-*")
	if err != nil {
		return fmt.Errorf("create tempdir: %w", err)
	}
	latest := filepath.Join(binDir, "crdb-sql")
	sibling := filepath.Join(binDir, benchSiblingQuarter.BackendName())
	if err := buildCrdbSQLBench(repoRoot, latest, "v262", ""); err != nil {
		return err
	}
	if err := buildCrdbSQLBench(repoRoot, sibling, "v261", "build/go.v261.mod"); err != nil {
		return err
	}
	if err := os.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
		return fmt.Errorf("prepend %s to PATH: %w", binDir, err)
	}
	return nil
}

func findRepoRootBench() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" {
		return "", fmt.Errorf("go env GOMOD returned empty; not inside a module")
	}
	return filepath.Dir(gomod), nil
}

func buildCrdbSQLBench(repoRoot, outputPath, quarter, modfile string) error {
	args := []string{"build"}
	if modfile != "" {
		args = append(args, "-modfile="+modfile)
	}
	args = append(args,
		"-ldflags=-X github.com/spilchen/sql-ai-tools/internal/versionroute.builtQuarterStamp="+quarter,
		"-o", outputPath, "./cmd/crdb-sql")
	cmd := exec.Command("go", args...)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go build (quarter=%s, modfile=%s) failed: %s: %w", quarter, modfile, out, err)
	}
	return nil
}

// newBenchRequest constructs the parse_sql call shared by every
// benchmark. "SELECT 1" is the smallest workload that exercises the
// full parse → envelope → JSON-RPC round trip without dragging in
// parser cold-start cost — that cost is paid once per child process
// regardless of router shape and is explicitly out of scope for #146.
func newBenchRequest() mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Name = "parse_sql"
	req.Params.Arguments = map[string]any{"sql": "SELECT 1"}
	return req
}

// requireSiblingResult asserts that res came from the v26.1 sibling
// by decoding the envelope and matching parser_version. Called once
// per benchmark on the warm-up Dispatch (not in the hot loop) — the
// goal is to catch wiring failures, not to bound the per-iteration
// cost.
func requireSiblingResult(b *testing.B, res *mcp.CallToolResult) {
	b.Helper()
	if res == nil {
		b.Fatal("nil CallToolResult")
	}
	if res.IsError {
		b.Fatalf("expected envelope-shaped success, got tool-error result: %+v", res.Content)
	}
	if len(res.Content) != 1 {
		b.Fatalf("expected 1 content item, got %d", len(res.Content))
	}
	txt, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		b.Fatalf("expected TextContent, got %T", res.Content[0])
	}
	var env output.Envelope
	if err := json.Unmarshal([]byte(txt.Text), &env); err != nil {
		b.Fatalf("decode envelope: %v: %s", err, txt.Text)
	}
	if env.ParserVersion != benchSiblingParserVersion {
		b.Fatalf("parser_version=%q, want %q (call hit the wrong sibling)",
			env.ParserVersion, benchSiblingParserVersion)
	}
}

// spawnPerCallRouter reconstructs the #129 SpawnRouter contract
// (spawn child, run MCP initialize, forward one tools/call, tear
// the child down) on top of the surviving spawnAndInit helper. The
// production SpawnRouter was deleted when PoolRouter replaced it
// in #145; this benchmark restores just enough of the contract to
// measure the spawn-per-call cost without putting SpawnRouter back
// into the production binary.
type spawnPerCallRouter struct{}

// Dispatch implements Router. Behavior matches the deleted
// SpawnRouter.Dispatch except for error wrapping: the bench fails
// loud on any error via b.Fatalf, so the original "spawn %s: %w" /
// "forward tools/call to %s (%s): %w" prose adds no value.
func (spawnPerCallRouter) Dispatch(
	ctx context.Context, want versionroute.Quarter, req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	path, found := versionroute.FindBackend(want)
	if !found {
		return missingBackendResult(want), nil
	}
	c, err := spawnAndInit(ctx, want, path, defaultInitTimeout)
	if err != nil {
		return nil, err
	}
	defer func() {
		// Mirror the deleted SpawnRouter: surface close errors to
		// stderr rather than swallow them. A close failure means the
		// sibling crashed or hung mid-call, and dropping that signal
		// hides process-leak regressions.
		if cerr := c.Close(); cerr != nil {
			log.Printf("crdb-sql bench: close sibling %s: %v", path, cerr)
		}
	}()
	return c.CallTool(ctx, req)
}

func BenchmarkRouteSpawnPerCall(b *testing.B) {
	if benchSetupErr != nil {
		b.Fatalf("bench setup failed: %v", benchSetupErr)
	}
	r := spawnPerCallRouter{}
	req := newBenchRequest()
	ctx := context.Background()

	res, err := r.Dispatch(ctx, benchSiblingQuarter, req)
	if err != nil {
		b.Fatalf("warm-up dispatch: %v", err)
	}
	requireSiblingResult(b, res)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Dispatch(ctx, benchSiblingQuarter, req); err != nil {
			b.Fatalf("dispatch %d: %v", i, err)
		}
	}
}

func BenchmarkRoutePooledWarm(b *testing.B) {
	if benchSetupErr != nil {
		b.Fatalf("bench setup failed: %v", benchSetupErr)
	}
	// WithIdleTimeout(0) disables the janitor goroutine entirely
	// (NewPoolRouter only starts it when idleTimeout > 0), so the
	// warm child stays in the pool for the whole run regardless of
	// how long the bench takes.
	p := NewPoolRouter(WithIdleTimeout(0))
	defer func() {
		if err := p.Close(); err != nil {
			b.Errorf("pool close: %v", err)
		}
	}()
	req := newBenchRequest()
	ctx := context.Background()

	res, err := p.Dispatch(ctx, benchSiblingQuarter, req)
	if err != nil {
		b.Fatalf("warm-up dispatch: %v", err)
	}
	requireSiblingResult(b, res)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.Dispatch(ctx, benchSiblingQuarter, req); err != nil {
			b.Fatalf("dispatch %d: %v", i, err)
		}
	}
}

func BenchmarkRoutePooledCold(b *testing.B) {
	if benchSetupErr != nil {
		b.Fatalf("bench setup failed: %v", benchSetupErr)
	}
	req := newBenchRequest()
	ctx := context.Background()

	// Verify once outside the loop with a throwaway pool so a wiring
	// regression fails fast instead of N times.
	{
		p := NewPoolRouter(WithIdleTimeout(0))
		res, err := p.Dispatch(ctx, benchSiblingQuarter, req)
		if err != nil {
			b.Fatalf("warm-up dispatch: %v", err)
		}
		requireSiblingResult(b, res)
		if err := p.Close(); err != nil {
			b.Fatalf("warm-up close: %v", err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := NewPoolRouter(WithIdleTimeout(0))
		if _, err := p.Dispatch(ctx, benchSiblingQuarter, req); err != nil {
			b.Fatalf("dispatch %d: %v", i, err)
		}
		if err := p.Close(); err != nil {
			b.Fatalf("close %d: %v", i, err)
		}
	}
}
