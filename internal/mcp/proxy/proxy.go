// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package proxy implements per-call routing of MCP tool calls from a
// long-lived `crdb-sql mcp` server to a sibling crdb-sql-vXXX backend
// whose parser quarter matches the caller's requested target_version.
//
// The router lives next to internal/mcp/routing.go: the wrapper
// decides whether routing is needed (resolved target_version's
// quarter differs from the running binary's), and on a positive
// answer hands off to a Router here. Sibling discovery reuses
// internal/versionroute (FindBackend, Discover, Quarter), so the
// CLI's startup-time MaybeReexec and the MCP server's per-call
// dispatch share one source of truth for "where do my siblings
// live?" and "what backend do I need for v26.1?".
//
// SpawnRouter is the first cut from issue #129: spawn the sibling
// per call, run the MCP initialize handshake, forward one tools/call,
// tear it down. The latency cost (process startup + handshake on
// every routed call) is documented; warm pooling is tracked in #145.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/spilchen/sql-ai-tools/internal/versionroute"
)

// Router dispatches an MCP tool call to a sibling crdb-sql backend
// whose parser quarter matches the caller's requested
// target_version. A successful Dispatch returns the sibling's
// *mcp.CallToolResult verbatim — including the sibling's
// parser_version stamp on the envelope, which is the visible signal
// that routing actually happened.
//
// Error contract: implementations distinguish two failure modes
// because the wrapper layer surfaces them differently to the
// caller.
//   - Transport failures (cannot spawn the sibling, broken pipe,
//     JSON-RPC framing error, initialize timeout, any error
//     returned by the sibling's MCP client other than an
//     IsError=true result) are returned as a non-nil Go error. The
//     wrapper propagates these as Go errors from the tool handler
//     so the MCP server reports a transport-layer failure rather
//     than fabricating an envelope.
//   - Tool-level failures (sibling answered with IsError=true, or
//     the requested sibling is not installed) are returned as a
//     non-nil *mcp.CallToolResult with IsError=true and a nil
//     error. The wrapper forwards these verbatim so the client
//     sees the same shape it would for a local tool error.
//
// SpawnRouter (and any future Router that constructs results
// itself) must NOT touch output.Envelope — those results bypass
// the local handler's envelope stamping entirely, so the saved
// "preserve env.Errors" rule does not apply here.
type Router interface {
	Dispatch(
		ctx context.Context, want versionroute.Quarter, req mcp.CallToolRequest,
	) (*mcp.CallToolResult, error)
}

// NoopRouter is the default Router used when per-call routing has
// not been wired into a server (e.g. unit tests for the wrapper that
// only care about the local-handler path, or a future build that
// disables routing). Every Dispatch returns a tool-error result
// rather than silently falling through to the local handler — silent
// fallback would mask the wiring bug this issue exists to prevent.
type NoopRouter struct{}

// Dispatch always returns a tool-error result naming the requested
// sibling. Returning IsError=true (rather than a Go error) lets the
// wrapper distinguish "no router configured" from "router blew up".
func (NoopRouter) Dispatch(
	_ context.Context, want versionroute.Quarter, _ mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultError(fmt.Sprintf(
		"per-call target_version routing is not enabled in this MCP server; "+
			"cannot serve %s", want.BackendName())), nil
}

// defaultInitTimeout caps the MCP initialize handshake on the
// child. Cold-process startup plus initialize can be noticeably
// slower than steady-state calls on loaded CI, so this is generous.
// Match the integration test's initTimeout in
// internal/mcp/integration_test.go so the two surfaces have the
// same flake floor.
const defaultInitTimeout = 10 * time.Second

// SpawnRouter implements Router by spawning a sibling backend as a
// child MCP server over stdio per call: locate the backend via
// versionroute.FindBackend, exec it with `mcp` as the subcommand,
// run the MCP initialize handshake, forward the single tools/call,
// and tear the child down via client.Close.
//
// This is the spawn-per-call first cut described in issue #129. The
// per-call cost is one process spawn plus one MCP handshake on top
// of the actual tool work; warm pooling that amortizes both is
// tracked in #145, with a benchmark in #146.
type SpawnRouter struct {
	// initTimeout caps the MCP initialize handshake on each spawned
	// child. The per-tool-call timeout flows from the ctx the
	// caller supplies to Dispatch.
	initTimeout time.Duration
}

// NewSpawnRouter returns a SpawnRouter with the package's default
// init timeout. Production callers (cmd/mcp.go) use this; tests that
// need a tighter handshake budget can construct SpawnRouter
// directly.
func NewSpawnRouter() *SpawnRouter {
	return &SpawnRouter{initTimeout: defaultInitTimeout}
}

// Dispatch spawns the sibling matching want, performs the MCP
// initialize handshake, forwards req, and returns the sibling's
// CallToolResult. See the Router interface comment for the
// transport-vs-tool error distinction.
func (r *SpawnRouter) Dispatch(
	ctx context.Context, want versionroute.Quarter, req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	path, found := versionroute.FindBackend(want)
	if !found {
		return missingBackendResult(want), nil
	}

	// nil env hands the parent's full environment to the child so any
	// per-tool environment knobs (DSNs, COCKROACH_BIN for explain_sql,
	// etc.) reach the sibling unchanged. The child also inherits
	// stderr, so its diagnostics are visible alongside the parent's —
	// load-bearing for debugging routed-call failures. Same reasoning
	// as internal/mcp/integration_test.go newMCPClient.
	c, err := client.NewStdioMCPClient(path, nil /* env */, "mcp")
	if err != nil {
		return nil, fmt.Errorf("spawn %s: %w", path, err)
	}
	defer func() {
		// Surface close errors to stderr rather than swallow them:
		// a Close failure typically means the sibling crashed or
		// hung mid-call, and silently dropping that signal hides
		// process-leak regressions. Use stderr (not the envelope)
		// because Dispatch has already returned its result by the
		// time this fires.
		if err := c.Close(); err != nil {
			fmt.Fprintf(os.Stderr,
				"crdb-sql mcp proxy: close sibling %s (%s): %v\n",
				want.BackendName(), path, err)
		}
	}()

	initCtx, cancel := context.WithTimeout(ctx, r.initTimeout)
	defer cancel()
	var initReq mcp.InitializeRequest
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{
		Name:    "crdb-sql-mcp-proxy",
		Version: "0.0.0",
	}
	if _, err := c.Initialize(initCtx, initReq); err != nil {
		// Distinguish "init budget exhausted" from "init failed for
		// some other reason" so the operator-facing message names
		// the budget rather than leaving the cause as a generic
		// "context deadline exceeded".
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf(
				"initialize %s (%s): exceeded init timeout %s: %w",
				want.BackendName(), path, r.initTimeout, err)
		}
		return nil, fmt.Errorf("initialize %s (%s): %w",
			want.BackendName(), path, err)
	}

	res, err := c.CallTool(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("forward tools/call to %s (%s): %w",
			want.BackendName(), path, err)
	}
	return res, nil
}

// missingBackendResult builds a tool-error CallToolResult whose
// message mirrors versionroute.writeMissingBackendError on the CLI:
// names the requested backend, reports what this binary is, and
// lists every discovered backend. The agent reading the result can
// then decide whether to install the missing sibling or drop the
// target_version override. Returning a tool-error result (rather
// than a Go transport error) keeps the failure visible at the same
// layer as a local handler's parameter validation error.
func missingBackendResult(want versionroute.Quarter) *mcp.CallToolResult {
	var sb strings.Builder
	fmt.Fprintf(&sb, "target_version requires the %s backend, which is not installed "+
		"alongside this binary or in $PATH.\n", want.BackendName())
	if built, ok := versionroute.Built(); ok {
		fmt.Fprintf(&sb, "This MCP server is %s.\n", built.BackendName())
	} else {
		sb.WriteString("This MCP server's quarter is unknown.\n")
	}
	backends := versionroute.Discover()
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
	fmt.Fprintf(&sb, "Install the %s backend from the GitHub release, or omit "+
		"target_version to use the bundled parser.", want.Tag())
	return mcp.NewToolResultError(sb.String())
}
