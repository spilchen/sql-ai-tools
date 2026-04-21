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

	"github.com/spilchen/sql-ai-tools/internal/risk"
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
	tools := s.ListTools()

	for _, name := range []string{PingToolName, ParseSQLToolName, DetectRiskyQueryToolName} {
		require.Contains(t, tools, name)
		require.NotNil(t, tools[name].Handler,
			"%s must be registered with a non-nil handler", name)
		require.NotEmpty(t, tools[name].Tool.Description,
			"%s description is part of the user-facing contract", name)
	}
}

// TestParseSQLHandler exercises the parse_sql tool handler directly,
// bypassing the MCP transport.
func TestParseSQLHandler(t *testing.T) {
	tests := []struct {
		name              string
		args              map[string]any
		expectedErr       bool
		expectedStmtCount int
		expectedType      string
		expectedTag       string
	}{
		{
			name:              "single DML",
			args:              map[string]any{"sql": "SELECT 1"},
			expectedStmtCount: 1,
			expectedType:      "DML",
			expectedTag:       "SELECT",
		},
		{
			name:              "multi-statement",
			args:              map[string]any{"sql": "SELECT 1; BEGIN"},
			expectedStmtCount: 2,
		},
		{
			name:        "parse error",
			args:        map[string]any{"sql": "SELECTT 1"},
			expectedErr: true,
		},
		{
			name:        "empty sql",
			args:        map[string]any{"sql": ""},
			expectedErr: true,
		},
		{
			name:        "missing sql param",
			args:        map[string]any{},
			expectedErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := parseSQLHandler("v0.26.2")
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args

			res, err := handler(context.Background(), req)
			require.NoError(t, err)
			require.NotNil(t, res)

			if tc.expectedErr {
				require.True(t, res.IsError, "expected tool-level error")
				return
			}

			require.False(t, res.IsError)
			require.Len(t, res.Content, 1)

			text, ok := res.Content[0].(mcpgo.TextContent)
			require.True(t, ok)

			var result parseSQLResult
			require.NoError(t, json.Unmarshal([]byte(text.Text), &result))
			require.Equal(t, "v0.26.2", result.ParserVersion)
			require.Len(t, result.Statements, tc.expectedStmtCount)

			if tc.expectedType != "" {
				require.Equal(t, tc.expectedType, string(result.Statements[0].StatementType))
				require.Equal(t, tc.expectedTag, result.Statements[0].Tag)
			}
		})
	}
}

// TestDetectRiskyQueryHandler exercises the detect_risky_query tool
// handler directly, bypassing the MCP transport.
func TestDetectRiskyQueryHandler(t *testing.T) {
	tests := []struct {
		name                 string
		args                 map[string]any
		expectedErr          bool
		expectedFindingCount int
		expectedReasonCode   string
	}{
		{
			name:                 "risky DELETE",
			args:                 map[string]any{"sql": "DELETE FROM users"},
			expectedFindingCount: 1,
			expectedReasonCode:   "DELETE_NO_WHERE",
		},
		{
			name:                 "safe SELECT",
			args:                 map[string]any{"sql": "SELECT id FROM t WHERE id = 1"},
			expectedFindingCount: 0,
		},
		{
			name:                 "multiple findings",
			args:                 map[string]any{"sql": "DELETE FROM t; SELECT * FROM t"},
			expectedFindingCount: 2,
		},
		{
			name:        "parse error",
			args:        map[string]any{"sql": "SELECTT 1"},
			expectedErr: true,
		},
		{
			name:        "empty sql",
			args:        map[string]any{"sql": ""},
			expectedErr: true,
		},
		{
			name:        "missing sql param",
			args:        map[string]any{},
			expectedErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := detectRiskyQueryHandler("v0.26.2")
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args

			res, err := handler(context.Background(), req)
			require.NoError(t, err)
			require.NotNil(t, res)

			if tc.expectedErr {
				require.True(t, res.IsError, "expected tool-level error")
				return
			}

			require.False(t, res.IsError)
			require.Len(t, res.Content, 1)

			text, ok := res.Content[0].(mcpgo.TextContent)
			require.True(t, ok)

			var result detectRiskyQueryResult
			require.NoError(t, json.Unmarshal([]byte(text.Text), &result))
			require.Equal(t, "v0.26.2", result.ParserVersion)
			require.Len(t, result.Findings, tc.expectedFindingCount)

			if tc.expectedReasonCode != "" {
				require.Equal(t, tc.expectedReasonCode, result.Findings[0].ReasonCode)
				require.Equal(t, risk.SeverityCritical, result.Findings[0].Severity)
				require.NotEmpty(t, result.Findings[0].FixHint)
			}
		})
	}
}
