// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package tools

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/output"
)

// TestSimulateSQLHandlerParameterValidation covers the tool-level
// error path for simulate_sql. Mirrors the explain_sql parameter
// table — a missing or wrong-typed required argument must produce a
// tool error, not an envelope.
func TestSimulateSQLHandlerParameterValidation(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
	}{
		{name: "missing both params", args: map[string]any{}},
		{name: "missing dsn", args: map[string]any{"sql": "SELECT 1"}},
		{name: "missing sql", args: map[string]any{"dsn": "postgres://h:26257/db"}},
		{name: "empty sql", args: map[string]any{"sql": "", "dsn": "postgres://h:26257/db"}},
		{name: "empty dsn", args: map[string]any{"sql": "SELECT 1", "dsn": ""}},
		{name: "wrong type sql", args: map[string]any{"sql": 1, "dsn": "postgres://h:26257/db"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := SimulateSQLHandler(testParserVersion, "" /* defaultTargetVersion */)
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args

			res, err := handler(context.Background(), req)
			require.NoError(t, err)
			require.NotNil(t, res)
			require.True(t, res.IsError, "expected tool-level error")
		})
	}
}

// TestSimulateSQLHandlerSafetyRejection verifies that the OpSimulate
// allowlist intercepts TCL/DCL/nested-EXPLAIN inputs before any
// cluster contact, in the MCP surface. Mirror of the CLI test in
// cmd/simulate_test.go.
func TestSimulateSQLHandlerSafetyRejection(t *testing.T) {
	tests := []struct {
		name        string
		sql         string
		expectedTag string
	}{
		{name: "begin rejected", sql: "BEGIN", expectedTag: "BEGIN"},
		{name: "grant rejected", sql: "GRANT SELECT ON t TO bob", expectedTag: "GRANT"},
		{name: "nested explain rejected", sql: "EXPLAIN SELECT 1", expectedTag: "EXPLAIN"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := SimulateSQLHandler(testParserVersion, "" /* defaultTargetVersion */)
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = map[string]any{
				"sql": tc.sql,
				// Unreachable on purpose: a regression that lets
				// the safety check fall through would surface here
				// as a connect error instead of safety_violation.
				"dsn": "postgres://nope:1/db?connect_timeout=1",
			}

			res, err := handler(context.Background(), req)
			require.NoError(t, err)
			env := requireEnvelope(t, res)
			require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus,
				"safety rejection must short-circuit before any cluster contact")
			require.Len(t, env.Errors, 1)
			require.Equal(t, output.CodeSafetyViolation, env.Errors[0].Code)
			require.Equal(t, tc.expectedTag, env.Errors[0].Context["tag"])
			require.Equal(t, "simulate", env.Errors[0].Context["operation"])
		})
	}
}

// TestSimulateSQLHandlerAdmitsDispatchableShapes is the inverse:
// SELECT, DML writes, and DDL all pass the safety gate and reach
// the connect step, even under the default read_only mode. The
// connect failure (unreachable DSN) is the assertion that the
// safety gate did not short-circuit.
func TestSimulateSQLHandlerAdmitsDispatchableShapes(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{name: "select admitted", sql: "SELECT 1"},
		{name: "insert admitted", sql: "INSERT INTO t VALUES (1)"},
		{name: "alter table admitted", sql: "ALTER TABLE t ADD COLUMN x INT"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := SimulateSQLHandler(testParserVersion, "" /* defaultTargetVersion */)
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = map[string]any{
				"sql": tc.sql,
				"dsn": "postgres://flaghost:26257/db?connect_timeout=1",
			}

			res, err := handler(context.Background(), req)
			require.NoError(t, err)
			env := requireEnvelope(t, res)
			require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)
			require.Len(t, env.Errors, 1)
			require.Contains(t, env.Errors[0].Message, "connect to CockroachDB",
				"dispatchable shape must reach the connect step under read_only mode")
		})
	}
}

// TestSimulateSQLHandlerRejectsInvalidMode covers the tool-level
// error path for a malformed mode argument.
func TestSimulateSQLHandlerRejectsInvalidMode(t *testing.T) {
	handler := SimulateSQLHandler(testParserVersion, "" /* defaultTargetVersion */)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"sql":  "SELECT 1",
		"dsn":  "postgres://nope:1/db",
		"mode": "yolo",
	}

	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.IsError, "invalid mode must produce a tool-level error, not an envelope")
}

// TestSimulateSQLToolAdvertisesParams pins the tool schema so a
// removed parameter (e.g. the agent-facing `mode` knob) fails
// loudly. We assert the parameter set rather than the exact
// description text.
func TestSimulateSQLToolAdvertisesParams(t *testing.T) {
	tool := SimulateSQLTool()
	require.Equal(t, SimulateSQLToolName, tool.Name)
	require.NotEmpty(t, tool.Description)

	// InputSchema.Properties carries the per-parameter schema. The
	// required set lives at InputSchema.Required.
	require.Contains(t, tool.InputSchema.Properties, "sql")
	require.Contains(t, tool.InputSchema.Properties, "dsn")
	require.Contains(t, tool.InputSchema.Properties, ModeParamName)
	require.Contains(t, tool.InputSchema.Properties, StatementTimeoutParamName)
	require.Contains(t, tool.InputSchema.Properties, TargetVersionParamName)
	require.ElementsMatch(t, []string{"sql", "dsn"}, tool.InputSchema.Required)
}
