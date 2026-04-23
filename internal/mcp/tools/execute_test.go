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

// TestExecuteSQLHandlerParameterValidation covers the tool-level
// error path for execute_sql. Mirrors the explain_sql validation
// table; the two surfaces share extractRequiredString and
// resolveTargetVersion plumbing, but pinning the call shape here
// guards against accidental divergence (e.g. forgetting to call one
// of the resolvers in the new handler).
func TestExecuteSQLHandlerParameterValidation(t *testing.T) {
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
		{name: "wrong type max_rows", args: map[string]any{"sql": "SELECT 1", "dsn": "postgres://h:26257/db", "max_rows": "many"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := ExecuteSQLHandler(testParserVersion, "" /* defaultTargetVersion */)
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args

			res, err := handler(context.Background(), req)
			require.NoError(t, err)
			require.NotNil(t, res)
			require.True(t, res.IsError, "expected tool-level error")
		})
	}
}

// TestExecuteSQLHandlerRejectsInvalidMode pins the same tool-level
// error contract for --mode that explain_sql enforces: an unknown
// token must produce a clear "valid choices are…" error rather than
// a misleading envelope entry.
func TestExecuteSQLHandlerRejectsInvalidMode(t *testing.T) {
	handler := ExecuteSQLHandler(testParserVersion, "" /* defaultTargetVersion */)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"sql":  "SELECT 1",
		"dsn":  "postgres://nope:1/db",
		"mode": "yolo",
	}

	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.IsError, "invalid mode must produce a tool-level error")
}

// TestExecuteSQLHandlerSafetyRejection verifies that the read_only
// allowlist intercepts mutating statements before any cluster
// contact. The DSN is unreachable on purpose: a regression that
// lets the safety check fall through would surface as a connect
// error instead of the safety_violation we expect.
func TestExecuteSQLHandlerSafetyRejection(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		sql         string
		expectedTag string
	}{
		{name: "delete under read_only", mode: "read_only", sql: "DELETE FROM t WHERE id = 1", expectedTag: "DELETE"},
		{name: "ddl under safe_write", mode: "safe_write", sql: "DROP TABLE users", expectedTag: "DROP TABLE"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := ExecuteSQLHandler(testParserVersion, "" /* defaultTargetVersion */)
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = map[string]any{
				"sql":  tc.sql,
				"dsn":  "postgres://nope:1/db?connect_timeout=1",
				"mode": tc.mode,
			}

			res, err := handler(context.Background(), req)
			require.NoError(t, err)
			env := requireEnvelope(t, res)
			require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus,
				"safety rejection must short-circuit before any cluster contact")
			require.Len(t, env.Errors, 1)
			require.Equal(t, output.CodeSafetyViolation, env.Errors[0].Code)
			require.Equal(t, tc.expectedTag, env.Errors[0].Context["tag"])
			require.Equal(t, "execute", env.Errors[0].Context["operation"])
		})
	}
}

// TestExecuteSQLHandlerConnectionFailureSurfacesEnvelopeError pins
// the same connection-failure → envelope-error contract that
// explain_sql enforces. A read_only SELECT must reach the connect
// step and surface a "connect to CockroachDB" envelope error rather
// than a tool-level error.
func TestExecuteSQLHandlerConnectionFailureSurfacesEnvelopeError(t *testing.T) {
	handler := ExecuteSQLHandler(testParserVersion, "" /* defaultTargetVersion */)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"sql": "SELECT 1",
		"dsn": "postgres://nope:1/db?connect_timeout=1",
	}

	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	env := requireEnvelope(t, res)
	require.Equal(t, output.TierConnected, env.Tier)
	require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)
	require.Len(t, env.Errors, 1)
	require.Contains(t, env.Errors[0].Message, "connect to CockroachDB")
}

// TestExecuteSQLToolAdvertisesParams pins the tool's discoverable
// schema: clients must be able to see all the optional knobs without
// reading source.
func TestExecuteSQLToolAdvertisesParams(t *testing.T) {
	tool := ExecuteSQLTool()
	props := tool.InputSchema.Properties
	for _, want := range []string{"sql", "dsn", TargetVersionParamName, ModeParamName, StatementTimeoutParamName, MaxRowsParamName} {
		require.Contains(t, props, want, "execute_sql schema must advertise %q", want)
	}
}

// TestResolveMaxRows pins the documented contract on the max_rows
// resolver: missing → defaultMax; positive → cast to int; negative
// → 0 ("unlimited"); non-numeric → tool-level error. Each case is a
// distinct user-visible behaviour, and a regression in any one
// silently changes how guardrails behave.
func TestResolveMaxRows(t *testing.T) {
	const defaultMax = 1000
	tests := []struct {
		name              string
		args              map[string]any
		expectedMax       int
		expectedToolError bool
	}{
		{name: "missing returns default", args: map[string]any{}, expectedMax: defaultMax},
		{name: "explicit zero disables cap", args: map[string]any{"max_rows": float64(0)}, expectedMax: 0},
		{name: "negative clamps to zero", args: map[string]any{"max_rows": float64(-5)}, expectedMax: 0},
		{name: "positive forwarded as int", args: map[string]any{"max_rows": float64(50)}, expectedMax: 50},
		{name: "wrong type produces tool error", args: map[string]any{"max_rows": "many"}, expectedToolError: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args
			got, toolErr := resolveMaxRows(req, defaultMax)
			if tc.expectedToolError {
				require.NotNil(t, toolErr, "expected a tool-level error")
				return
			}
			require.Nil(t, toolErr)
			require.Equal(t, tc.expectedMax, got)
		})
	}
}
