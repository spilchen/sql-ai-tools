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

// TestNewServerRegistersTools locks in that NewServer wires all tools
// under their documented names. A future refactor that forgets to call
// AddTool — or renames a tool — would silently break MCP clients; this
// test surfaces it without speaking the protocol.
func TestNewServerRegistersTools(t *testing.T) {
	s := NewServer("v1.2.3", "v0.26.2")
	require.NotNil(t, s)
	registered := s.ListTools()

	for _, name := range []string{PingToolName, tools.ParseSQLToolName, tools.ValidateSQLToolName, tools.FormatSQLToolName, tools.DetectRiskyQueryToolName, tools.ExplainSQLToolName} {
		require.Contains(t, registered, name)
		require.NotNil(t, registered[name].Handler,
			"%s must be registered with a non-nil handler", name)
		require.NotEmpty(t, registered[name].Tool.Description,
			"%s description is part of the user-facing contract", name)
	}
}
