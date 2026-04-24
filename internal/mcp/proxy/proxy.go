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
// PoolRouter (pool.go) is the production implementation: one warm
// child per quarter, lazy spawn on first call, idle eviction after
// a configurable window, transparent re-spawn on transport failure.
// It implements issue #145 and replaces the spawn-per-call first
// cut from #129. NoopRouter is the default when no Router is
// installed; it surfaces a "routing not enabled" tool error rather
// than silently dispatching locally.
package proxy

import (
	"context"
	"errors"
	"fmt"
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
// A Router that constructs results itself (NoopRouter,
// missingBackendResult) must NOT touch output.Envelope — those
// results bypass the local handler's envelope stamping entirely, so
// the saved "preserve env.Errors" rule does not apply here.
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

// spawnAndInit launches the sibling at path as a child MCP server
// over stdio, runs the MCP initialize handshake against it under
// initTimeout, and returns the connected client. On any error the
// returned client is nil and the child has been torn down (no leak).
//
// nil env in NewStdioMCPClient hands the parent's full environment
// to the child so any per-tool environment knobs (DSNs,
// COCKROACH_BIN for explain_sql, etc.) reach the sibling unchanged.
// The child also inherits stderr, so its diagnostics are visible
// alongside the parent's — load-bearing for debugging routed-call
// failures. Same reasoning as internal/mcp/integration_test.go's
// newMCPClient.
//
// want is only used to compose a clear error message ("initialize
// crdb-sql-v261 (/usr/local/bin/crdb-sql-v261): ..."); path is the
// resolved binary location.
func spawnAndInit(
	ctx context.Context, want versionroute.Quarter, path string, initTimeout time.Duration,
) (*client.Client, error) {
	c, err := client.NewStdioMCPClient(path, nil /* env */, "mcp")
	if err != nil {
		return nil, fmt.Errorf("spawn %s: %w", path, err)
	}

	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	var initReq mcp.InitializeRequest
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{
		Name:    "crdb-sql-mcp-proxy",
		Version: "0.0.0",
	}
	if _, err := c.Initialize(initCtx, initReq); err != nil {
		// Tear the child down so a failed handshake does not leak a
		// running subprocess. Surface any close-time error by
		// joining it with the original — the operator needs both:
		// what failed (init) and any cascading shutdown trouble.
		closeErr := c.Close()
		// Distinguish "init budget exhausted" from "init failed for
		// some other reason" so the operator-facing message names
		// the budget rather than leaving the cause as a generic
		// "context deadline exceeded".
		var wrapped error
		if errors.Is(err, context.DeadlineExceeded) {
			wrapped = fmt.Errorf(
				"initialize %s (%s): exceeded init timeout %s: %w",
				want.BackendName(), path, initTimeout, err)
		} else {
			wrapped = fmt.Errorf("initialize %s (%s): %w",
				want.BackendName(), path, err)
		}
		if closeErr != nil {
			return nil, errors.Join(wrapped, fmt.Errorf("close after failed init: %w", closeErr))
		}
		return nil, wrapped
	}
	return c, nil
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
