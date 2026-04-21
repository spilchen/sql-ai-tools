// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package tools

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/sqlparse"
)

const testParserVersion = "v0.26.2"

// requireEnvelope asserts that res is a successful tool result (not a
// tool-level error), extracts the TextContent, and unmarshals it into
// an output.Envelope.
func requireEnvelope(t *testing.T, res *mcpgo.CallToolResult) output.Envelope {
	t.Helper()
	require.False(t, res.IsError, "expected successful tool result, got tool-level error")
	require.Len(t, res.Content, 1, "expected exactly one content block")
	text, ok := res.Content[0].(mcpgo.TextContent)
	require.True(t, ok, "expected TextContent, got %T", res.Content[0])
	var env output.Envelope
	require.NoError(t, json.Unmarshal([]byte(text.Text), &env))
	return env
}

func TestExtractSQL(t *testing.T) {
	tests := []struct {
		name        string
		args        map[string]any
		expectedSQL string
		expectedErr bool
	}{
		{name: "valid string", args: map[string]any{"sql": "SELECT 1"}, expectedSQL: "SELECT 1"},
		{name: "missing param", args: map[string]any{}, expectedErr: true},
		{name: "wrong type", args: map[string]any{"sql": 42}, expectedErr: true},
		{name: "empty string", args: map[string]any{"sql": ""}, expectedErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args

			sql, toolErr := extractSQL(req)
			if tc.expectedErr {
				require.NotNil(t, toolErr, "expected tool error")
				require.Empty(t, sql)
			} else {
				require.Nil(t, toolErr, "expected no tool error")
				require.Equal(t, tc.expectedSQL, sql)
			}
		})
	}
}

func TestParseSQLHandler(t *testing.T) {
	tests := []struct {
		name              string
		args              map[string]any
		expectedToolErr   bool
		expectedEnvErrs   bool
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
			name:            "parse error returns envelope errors",
			args:            map[string]any{"sql": "SELECTT 1"},
			expectedEnvErrs: true,
		},
		{
			name:            "missing sql param",
			args:            map[string]any{},
			expectedToolErr: true,
		},
		{
			name:            "empty sql",
			args:            map[string]any{"sql": ""},
			expectedToolErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := ParseSQLHandler(testParserVersion)
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args

			res, err := handler(context.Background(), req)
			require.NoError(t, err)
			require.NotNil(t, res)

			if tc.expectedToolErr {
				require.True(t, res.IsError, "expected tool-level error")
				return
			}

			env := requireEnvelope(t, res)
			require.Equal(t, testParserVersion, env.ParserVersion)
			require.Equal(t, output.TierZeroConfig, env.Tier)
			require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)

			if tc.expectedEnvErrs {
				require.NotEmpty(t, env.Errors)
				require.Nil(t, env.Data)
				return
			}

			require.Empty(t, env.Errors)
			var stmts []sqlparse.ClassifiedStatement
			require.NoError(t, json.Unmarshal(env.Data, &stmts))
			require.Len(t, stmts, tc.expectedStmtCount)

			if tc.expectedType != "" {
				require.Equal(t, tc.expectedType, string(stmts[0].StatementType))
				require.Equal(t, tc.expectedTag, stmts[0].Tag)
			}
		})
	}
}

func TestValidateSQLHandler(t *testing.T) {
	tests := []struct {
		name            string
		args            map[string]any
		expectedToolErr bool
		expectedValid   bool
		expectedEnvErrs bool
		expectedCode    string
	}{
		{
			name:          "valid SQL",
			args:          map[string]any{"sql": "SELECT 1"},
			expectedValid: true,
		},
		{
			name:            "syntax error",
			args:            map[string]any{"sql": "SELECT FROM"},
			expectedEnvErrs: true,
			expectedCode:    "42601",
		},
		{
			name:            "type mismatch",
			args:            map[string]any{"sql": "SELECT 1 + 'hello'"},
			expectedEnvErrs: true,
		},
		{
			name:          "column ref does not false-positive",
			args:          map[string]any{"sql": "SELECT a + 1 FROM t"},
			expectedValid: true,
		},
		{
			name:          "whitespace trimmed",
			args:          map[string]any{"sql": "  SELECT 1  \n"},
			expectedValid: true,
		},
		{
			name:            "missing sql param",
			args:            map[string]any{},
			expectedToolErr: true,
		},
		{
			name:            "empty sql",
			args:            map[string]any{"sql": ""},
			expectedToolErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := ValidateSQLHandler(testParserVersion)
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args

			res, err := handler(context.Background(), req)
			require.NoError(t, err)
			require.NotNil(t, res)

			if tc.expectedToolErr {
				require.True(t, res.IsError, "expected tool-level error")
				return
			}

			env := requireEnvelope(t, res)
			require.Equal(t, testParserVersion, env.ParserVersion)
			require.Equal(t, output.TierZeroConfig, env.Tier)
			require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)

			if tc.expectedEnvErrs {
				require.NotEmpty(t, env.Errors)
				require.Nil(t, env.Data)
				if tc.expectedCode != "" {
					require.Equal(t, tc.expectedCode, env.Errors[0].Code)
				}
				return
			}

			require.Empty(t, env.Errors)
			require.NotNil(t, env.Data)

			if tc.expectedValid {
				var data struct {
					Valid bool `json:"valid"`
				}
				require.NoError(t, json.Unmarshal(env.Data, &data))
				require.True(t, data.Valid)
			}
		})
	}
}

func TestFormatSQLHandler(t *testing.T) {
	tests := []struct {
		name              string
		args              map[string]any
		expectedToolErr   bool
		expectedEnvErrs   bool
		expectedFormatted string
	}{
		{
			name:              "basic formatting",
			args:              map[string]any{"sql": "select  1"},
			expectedFormatted: "SELECT 1",
		},
		{
			name:              "multi-statement",
			args:              map[string]any{"sql": "select 1; select 2"},
			expectedFormatted: "SELECT 1;\nSELECT 2",
		},
		{
			name:            "parse error",
			args:            map[string]any{"sql": "SELECTT 1"},
			expectedEnvErrs: true,
		},
		{
			name:            "missing sql param",
			args:            map[string]any{},
			expectedToolErr: true,
		},
		{
			name:            "empty sql",
			args:            map[string]any{"sql": ""},
			expectedToolErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := FormatSQLHandler(testParserVersion)
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args

			res, err := handler(context.Background(), req)
			require.NoError(t, err)
			require.NotNil(t, res)

			if tc.expectedToolErr {
				require.True(t, res.IsError, "expected tool-level error")
				return
			}

			env := requireEnvelope(t, res)
			require.Equal(t, testParserVersion, env.ParserVersion)
			require.Equal(t, output.TierZeroConfig, env.Tier)
			require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)

			if tc.expectedEnvErrs {
				require.NotEmpty(t, env.Errors)
				require.Nil(t, env.Data)
				return
			}

			require.Empty(t, env.Errors)
			require.NotNil(t, env.Data)

			var data struct {
				FormattedSQL string `json:"formatted_sql"`
			}
			require.NoError(t, json.Unmarshal(env.Data, &data))
			require.Equal(t, tc.expectedFormatted, data.FormattedSQL)
		})
	}
}
