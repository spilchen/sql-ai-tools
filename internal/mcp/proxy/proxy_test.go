// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package proxy

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/versionroute"
)

// fakeQuarter is a Year.Quarter combination that is extremely
// unlikely to be installed on any developer machine. Tests that
// exercise missing-backend behavior target it so they do not depend
// on the host having (or not having) any specific sibling binary.
var fakeQuarter = versionroute.Quarter{Year: 99, Q: 4}

// requireToolError asserts that res is a tool-level error and
// returns the concatenated text of every TextContent block so the
// caller can run substring assertions.
func requireToolError(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	require.NotNil(t, res, "expected a result, got nil")
	require.True(t, res.IsError, "expected tool-level error result")
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			text += tc.Text
		}
	}
	require.NotEmpty(t, text, "tool error result must carry a message")
	return text
}

// TestNoopRouterReturnsToolError pins that the default Router does
// not silently fall through. A wiring regression that left NoopRouter
// in place where PoolRouter was intended must surface as a clear
// "routing not enabled" message rather than a confusing "answered
// with the wrong parser" envelope.
func TestNoopRouterReturnsToolError(t *testing.T) {
	res, err := NoopRouter{}.Dispatch(context.Background(), fakeQuarter, mcp.CallToolRequest{})
	require.NoError(t, err)
	text := requireToolError(t, res)
	require.Contains(t, text, "not enabled")
	require.Contains(t, text, fakeQuarter.BackendName())
}
