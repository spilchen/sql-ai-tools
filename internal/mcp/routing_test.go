// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/mcp/proxy"
	"github.com/spilchen/sql-ai-tools/internal/versionroute"
)

// fakeRouter records each Dispatch call and returns canned
// responses. Tests use it to verify the wrapper's routing decision
// without spawning a real subprocess.
type fakeRouter struct {
	dispatched []dispatchCall
	result     *mcp.CallToolResult
	err        error
}

type dispatchCall struct {
	want versionroute.Quarter
	req  mcp.CallToolRequest
}

func (f *fakeRouter) Dispatch(
	_ context.Context, want versionroute.Quarter, req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	f.dispatched = append(f.dispatched, dispatchCall{want: want, req: req})
	return f.result, f.err
}

// localHandlerSentinel is the response the recordingHandler returns
// to the wrapper. Tests assert on its identity to confirm the local
// path was taken (vs. a proxied response from fakeRouter).
var localHandlerSentinel = mcp.NewToolResultText(`{"local":true}`)

// recordingHandler is the inner handler the wrapper decorates. It
// records each call and returns a fixed sentinel result so tests
// can distinguish "local handler ran" from "router ran".
type recordingHandler struct {
	calls int
}

func (r *recordingHandler) handle(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	r.calls++
	return localHandlerSentinel, nil
}

// builtV262 and builtV261 are the canonical "running binary's
// quarter" fixtures used across the tests. v262 mirrors the
// production latest binary; v261 mirrors the sibling installed for
// cross-version routing.
var (
	builtV262 = versionroute.Quarter{Year: 26, Q: 2}
	builtV261 = versionroute.Quarter{Year: 26, Q: 1}
)

// callRequest builds a tools/call request from a flat argument map.
// The wrapper does not care about the tool name field, so tests
// leave it unset.
func callRequest(args map[string]any) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Arguments = args
	return req
}

func TestWithRoutingDispatchDecision(t *testing.T) {
	tests := []struct {
		name                  string
		built                 versionroute.Quarter
		defaultTargetVersion  string
		args                  map[string]any
		expectRouted          bool
		expectLocalCalls      int
		expectRouterCalls     int
		expectRouterWantQuart versionroute.Quarter
	}{
		{
			name:             "no target_version uses local handler",
			built:            builtV262,
			args:             map[string]any{"sql": "SELECT 1"},
			expectLocalCalls: 1,
		},
		{
			name:                 "matching default uses local handler",
			built:                builtV262,
			defaultTargetVersion: "26.2.0",
			args:                 map[string]any{"sql": "SELECT 1"},
			expectLocalCalls:     1,
		},
		{
			name:             "matching per-call target uses local handler",
			built:            builtV262,
			args:             map[string]any{"sql": "SELECT 1", "target_version": "26.2.0"},
			expectLocalCalls: 1,
		},
		{
			name:                  "different per-call target routes to sibling",
			built:                 builtV262,
			args:                  map[string]any{"sql": "SELECT 1", "target_version": "26.1.0"},
			expectRouted:          true,
			expectRouterCalls:     1,
			expectRouterWantQuart: builtV261,
		},
		{
			name:                  "different default routes to sibling",
			built:                 builtV262,
			defaultTargetVersion:  "26.1.0",
			args:                  map[string]any{"sql": "SELECT 1"},
			expectRouted:          true,
			expectRouterCalls:     1,
			expectRouterWantQuart: builtV261,
		},
		{
			name:                  "per-call beats default and routes to per-call quarter",
			built:                 builtV262,
			defaultTargetVersion:  "26.2.0",
			args:                  map[string]any{"sql": "SELECT 1", "target_version": "26.1.0"},
			expectRouted:          true,
			expectRouterCalls:     1,
			expectRouterWantQuart: builtV261,
		},
		{
			name:             "unknown built quarter falls through to local",
			built:            versionroute.Quarter{},
			args:             map[string]any{"sql": "SELECT 1", "target_version": "26.1.0"},
			expectLocalCalls: 1,
		},
		{
			name:             "leading v on per-call target normalizes to same quarter",
			built:            builtV262,
			args:             map[string]any{"sql": "SELECT 1", "target_version": "v26.2.0"},
			expectLocalCalls: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rh := &recordingHandler{}
			fr := &fakeRouter{result: mcp.NewToolResultText(`{"routed":true}`)}

			wrapped := withRouting(rh.handle, tc.defaultTargetVersion, tc.built, fr)
			res, err := wrapped(context.Background(), callRequest(tc.args))
			require.NoError(t, err)
			require.NotNil(t, res)

			require.Equal(t, tc.expectLocalCalls, rh.calls, "local handler invocation count")
			require.Len(t, fr.dispatched, tc.expectRouterCalls, "router invocation count")
			if tc.expectRouted {
				require.Equal(t, tc.expectRouterWantQuart, fr.dispatched[0].want,
					"router was dispatched with the wrong quarter")
				require.Same(t, fr.result, res,
					"wrapper must return the router's result verbatim")
			} else {
				require.Same(t, localHandlerSentinel, res,
					"wrapper must return the local handler's result on the local path")
			}
		})
	}
}

// TestWithRoutingMalformedTargetVersionPreservesValidationError pins
// that a per-call target_version that fails ResolveTargetVersion's
// validation surfaces as a tool error from the wrapper, identical to
// what the local handler would have produced. Routing must not eat a
// parameter validation problem just because it intercepts the
// resolution step.
func TestWithRoutingMalformedTargetVersionPreservesValidationError(t *testing.T) {
	rh := &recordingHandler{}
	fr := &fakeRouter{result: mcp.NewToolResultText(`{"routed":true}`)}

	wrapped := withRouting(rh.handle, "" /* defaultTargetVersion */, builtV262, fr)
	res, err := wrapped(context.Background(), callRequest(map[string]any{
		"sql":            "SELECT 1",
		"target_version": "garbage",
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.True(t, res.IsError, "malformed target_version must surface as a tool error")
	require.Zero(t, rh.calls, "local handler must not run when validation fails")
	require.Empty(t, fr.dispatched, "router must not run when validation fails")
}

// TestWithRoutingTransportErrorPropagates pins that a Go-error
// return from the router (a transport failure: spawn failed, broken
// pipe, framing error) propagates as the wrapper's Go-error return.
// The MCP server treats this as a transport-layer failure rather
// than fabricating an envelope.
func TestWithRoutingTransportErrorPropagates(t *testing.T) {
	rh := &recordingHandler{}
	transportErr := errors.New("spawn failed")
	fr := &fakeRouter{err: transportErr}

	wrapped := withRouting(rh.handle, "", builtV262, fr)
	res, err := wrapped(context.Background(), callRequest(map[string]any{
		"sql":            "SELECT 1",
		"target_version": "26.1.0",
	}))
	require.ErrorIs(t, err, transportErr, "wrapper must propagate transport errors")
	require.Nil(t, res, "no result on transport failure")
	require.Zero(t, rh.calls, "local handler must not run on transport failure")
}

// TestWithRoutingToolErrorPropagatesAsResult pins that an
// IsError=true result from the router (e.g. NoopRouter or
// missing-backend) reaches the client unchanged. The wrapper must
// not coerce it into a Go error; that would lose the user-facing
// message.
func TestWithRoutingToolErrorPropagatesAsResult(t *testing.T) {
	rh := &recordingHandler{}
	toolErrResult := mcp.NewToolResultError("missing backend")
	fr := &fakeRouter{result: toolErrResult}

	wrapped := withRouting(rh.handle, "", builtV262, fr)
	res, err := wrapped(context.Background(), callRequest(map[string]any{
		"sql":            "SELECT 1",
		"target_version": "26.1.0",
	}))
	require.NoError(t, err, "tool errors must not surface as Go errors")
	require.Same(t, toolErrResult, res, "tool error result must reach the client unchanged")
	require.Zero(t, rh.calls, "local handler must not run when routing fires")
}

// TestWithRoutingNoopRouterReturnsToolError sanity-checks the
// wrapper + NoopRouter composition end-to-end: a different-quarter
// call against an unrouted server produces NoopRouter's "not
// enabled" message.
func TestWithRoutingNoopRouterReturnsToolError(t *testing.T) {
	rh := &recordingHandler{}
	wrapped := withRouting(rh.handle, "", builtV262, proxy.NoopRouter{})
	res, err := wrapped(context.Background(), callRequest(map[string]any{
		"sql":            "SELECT 1",
		"target_version": "26.1.0",
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.True(t, res.IsError)
	require.Zero(t, rh.calls)
}

func TestRouteTarget(t *testing.T) {
	tests := []struct {
		name         string
		built        versionroute.Quarter
		target       string
		expectedWant versionroute.Quarter
		expectedOK   bool
	}{
		{name: "empty target", built: builtV262, target: ""},
		{name: "unparseable target", built: builtV262, target: "garbage"},
		{name: "unknown built quarter", built: versionroute.Quarter{}, target: "26.1.0"},
		{name: "matching quarter", built: builtV262, target: "26.2.0"},
		{name: "matching quarter with leading v", built: builtV262, target: "v26.2"},
		{
			name:  "different quarter routes and returns parsed quarter",
			built: builtV262, target: "26.1.0",
			expectedWant: builtV261, expectedOK: true,
		},
		{
			name:  "different year routes",
			built: builtV262, target: "25.4.0",
			expectedWant: versionroute.Quarter{Year: 25, Q: 4}, expectedOK: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want, ok := routeTarget(tc.built, tc.target)
			require.Equal(t, tc.expectedOK, ok)
			require.Equal(t, tc.expectedWant, want)
		})
	}
}
