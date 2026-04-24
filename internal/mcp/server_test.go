// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/mcp/tools"
	"github.com/spilchen/sql-ai-tools/internal/versionroute"
)

// TestPingHandler exercises the ping tool handler directly, bypassing
// the MCP transport. The handler must always return ok=true and echo
// back the parser version it was constructed with — that's the contract
// clients (and the README's verification step) rely on.
func TestPingHandler(t *testing.T) {
	tests := []struct {
		name                  string
		parserVersion         string
		expectedParserVersion string
	}{
		{
			name:                  "stamped semver",
			parserVersion:         "v0.26.2",
			expectedParserVersion: "v0.26.2",
		},
		{
			name:                  "dev fallback string",
			parserVersion:         "unknown",
			expectedParserVersion: "unknown",
		},
		{
			name:                  "empty version is passed through verbatim",
			parserVersion:         "",
			expectedParserVersion: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := pingHandler(tc.parserVersion)
			res, err := handler(context.Background(), mcpgo.CallToolRequest{})
			require.NoError(t, err)
			require.NotNil(t, res)
			require.False(t, res.IsError, "ping must not report a tool-level error")
			require.Len(t, res.Content, 1, "ping should emit a single content block")

			text, ok := res.Content[0].(mcpgo.TextContent)
			require.True(t, ok, "ping content should be TextContent, got %T", res.Content[0])

			// Pin the exact wire shape — round-tripping through pingResult
			// would mask a json-tag rename (e.g. parser_version ->
			// parserVersion), which is the contract change pingResult's
			// doc warns about.
			expectedJSON, err := json.Marshal(map[string]any{
				"ok":             true,
				"parser_version": tc.expectedParserVersion,
			})
			require.NoError(t, err)
			require.JSONEq(t, string(expectedJSON), text.Text)
		})
	}
}

// spyRouter is a test-only proxy.Router that records every Dispatch
// call so server-wiring tests can assert that the routing wrapper
// fired (or did not fire) for a given tool.
type spyRouter struct {
	calls int
}

func (s *spyRouter) Dispatch(
	_ context.Context, _ versionroute.Quarter, _ mcpgo.CallToolRequest,
) (*mcpgo.CallToolResult, error) {
	s.calls++
	// Return a benign success result so the wrapper has something
	// to forward; the test only cares that Dispatch was called.
	return mcpgo.NewToolResultText(`{"routed":true}`), nil
}

// TestNewServerRoutesAllParserDependentToolsAndOnlyThem pins the
// wiring contract from server.go's doc: the nine parser-dependent
// handlers must be wrapped with withRouting, and the three that do
// not take target_version must NOT be wrapped. A future refactor
// that drops `route(...)` from one AddTool line — or wraps a tool
// that does not accept target_version — would silently break the
// per-call routing contract from issue #129. This test catches both
// shapes.
func TestNewServerRoutesAllParserDependentToolsAndOnlyThem(t *testing.T) {
	const (
		serverQuarter = "26.2.0"
		routedTarget  = "26.1.0"
	)
	built := versionroute.Quarter{Year: 26, Q: 2}

	wrapped := []string{
		tools.ParseSQLToolName,
		tools.ValidateSQLToolName,
		tools.FormatSQLToolName,
		tools.DetectRiskyQueryToolName,
		tools.SummarizeSQLToolName,
		tools.ExplainSQLToolName,
		tools.ExplainSchemaChangeToolName,
		tools.SimulateSQLToolName,
		tools.ExecuteSQLToolName,
	}
	unwrapped := []string{
		PingToolName,
		tools.ListTablesToolName,
		tools.DescribeTableToolName,
	}

	// One CallToolRequest per tool — handlers reject missing
	// required params even when the wrapper would have routed, so
	// the test passes plausible-but-unused values for required
	// arguments. The router-spy fires before any local-handler
	// validation runs, so for the wrapped set we never reach the
	// local handler at all.
	args := map[string]any{
		"sql":            "SELECT 1",
		"dsn":            "postgres://nope:1/db?connect_timeout=1",
		"target_version": routedTarget,
		"table":          "t",
		"schemas":        []any{},
	}

	for _, name := range wrapped {
		t.Run("wrapped/"+name, func(t *testing.T) {
			spy := &spyRouter{}
			s := NewServer("v1.2.3", "v0.26.2", serverQuarter,
				WithRouter(spy), WithBuiltQuarter(built))
			handler := s.ListTools()[name].Handler
			require.NotNil(t, handler)

			req := mcpgo.CallToolRequest{}
			req.Params.Name = name
			req.Params.Arguments = args
			_, err := handler(context.Background(), req)
			require.NoError(t, err)
			require.Equal(t, 1, spy.calls,
				"%s must route to the sibling on a different-quarter target_version", name)
		})
	}

	for _, name := range unwrapped {
		t.Run("unwrapped/"+name, func(t *testing.T) {
			spy := &spyRouter{}
			s := NewServer("v1.2.3", "v0.26.2", serverQuarter,
				WithRouter(spy), WithBuiltQuarter(built))
			handler := s.ListTools()[name].Handler
			require.NotNil(t, handler)

			req := mcpgo.CallToolRequest{}
			req.Params.Name = name
			req.Params.Arguments = args
			// Some unwrapped handlers (list_tables, describe_table)
			// require their own arguments; we only care here that
			// they do NOT route, not that they succeed.
			_, _ = handler(context.Background(), req)
			require.Zero(t, spy.calls,
				"%s must NOT be wrapped — it does not accept target_version", name)
		})
	}
}

// TestNewServerRegistersTools locks in that NewServer wires all tools
// under their documented names. A future refactor that forgets to call
// AddTool — or renames a tool — would silently break MCP clients; this
// test surfaces it without speaking the protocol.
func TestNewServerRegistersTools(t *testing.T) {
	s := NewServer("v1.2.3", "v0.26.2", "" /* defaultTargetVersion */)
	require.NotNil(t, s)
	registered := s.ListTools()

	for _, name := range []string{
		PingToolName,
		tools.ParseSQLToolName,
		tools.ValidateSQLToolName,
		tools.FormatSQLToolName,
		tools.DetectRiskyQueryToolName,
		tools.ExplainSQLToolName,
		tools.ExplainSchemaChangeToolName,
		tools.SimulateSQLToolName,
		tools.ListTablesToolName,
		tools.DescribeTableToolName,
	} {
		require.Contains(t, registered, name)
		require.NotNil(t, registered[name].Handler,
			"%s must be registered with a non-nil handler", name)
		require.NotEmpty(t, registered[name].Tool.Description,
			"%s description is part of the user-facing contract", name)
	}
}
