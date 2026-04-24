// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/spilchen/sql-ai-tools/internal/mcp/proxy"
	"github.com/spilchen/sql-ai-tools/internal/mcp/tools"
	"github.com/spilchen/sql-ai-tools/internal/versionroute"
)

// withRouting wraps h so that any tool call whose resolved
// target_version maps to a different versionroute.Quarter than the
// running binary's `built` is forwarded to router instead of
// executed locally. Tools that do not accept target_version (ping,
// list_tables, describe_table) must not be wrapped — wrapping them
// would silently drop the parameter.
//
// The wrapper preserves the exact error contract callers depend on:
//   - Parameter validation errors from the target_version field
//     surface as tool-level errors (IsError=true) just as they would
//     from the local handler. Routing must not eat a parameter
//     validation problem.
//   - When the router returns (nil, error), the wrapper propagates
//     the Go error so the MCP server reports it as transport-layer.
//   - When the router returns a result, the wrapper returns it
//     verbatim so the sibling's parser_version reaches the client.
//
// When built is the zero Quarter (an unstamped local build with no
// parser dep in BuildInfo, or a `go test` binary), the wrapper
// always falls through to the local handler. There is no meaningful
// quarter to compare against, and per-handler stamping
// (output.VersionMismatchWarning via stampTargetVersion) still
// informs the client of the skew. A malformed builtQuarterStamp
// that turns Built() into the zero Quarter is surfaced separately
// at process startup by versionroute.MaybeReexec, which prints
// versionroute.StampDiagnostic to stderr — so the operator sees
// the build defect even when the MCP server itself stays quiet.
//
// The double-call to tools.ResolveTargetVersion (once here, once
// inside the local handler when routing falls through) is benign
// today because the function is idempotent. If a future change
// introduces a side effect there, the local handler should accept
// a pre-resolved target_version instead of re-resolving.
func withRouting(
	h server.ToolHandlerFunc,
	defaultTargetVersion string,
	built versionroute.Quarter,
	router proxy.Router,
) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		target, toolErr := tools.ResolveTargetVersion(req, defaultTargetVersion)
		if toolErr != nil {
			return toolErr, nil
		}
		if want, ok := routeTarget(built, target); ok {
			return router.Dispatch(ctx, want, req)
		}
		return h(ctx, req)
	}
}

// routeTarget decides whether a call with the given resolved target
// must be forwarded to a sibling, and on a positive answer returns
// the parsed want Quarter so the caller does not re-parse. Folding
// the parse and the decision into one function eliminates the
// structural-coincidence hazard of having shouldRoute promise
// "parseable" while a separate ParseFromTarget call discards the
// ok bool.
//
// Returns (zero, false) — handle locally — for any of:
//   - empty target (no version requested)
//   - unparseable target (let the local handler's existing
//     validation produce the diagnostic)
//   - built quarter is unknown (no comparison possible)
//   - built quarter equals want quarter (same parser)
func routeTarget(built versionroute.Quarter, target string) (versionroute.Quarter, bool) {
	if target == "" {
		return versionroute.Quarter{}, false
	}
	want, ok := versionroute.ParseFromTarget(target)
	if !ok {
		return versionroute.Quarter{}, false
	}
	if built.IsZero() {
		return versionroute.Quarter{}, false
	}
	if built == want {
		return versionroute.Quarter{}, false
	}
	return want, true
}
