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

// messySQL is the verbatim transcript paste from issue #161. Every
// line carries a cockroach sql REPL prompt artifact (primary or
// continuation); the stripped form parses as a single SELECT.
const messySQL = "root@localhost:26257/movr> SELECT r.city,r.id AS ride_id,\n" +
	"                        -> u.name AS rider_name,v.type AS vehicle_type,\n" +
	"                        ->    r.start_address,r.end_address,\n" +
	"                        -> r.revenue FROM rides r INNER JOIN\n" +
	"                        -> users u ON r.rider_id=u.id AND r.city=u.city INNER JOIN vehicles v\n" +
	"                        ->       ON r.vehicle_id=v.id AND\n" +
	"                        -> r.vehicle_city=v.city WHERE r.city='new york'\n" +
	"                        ->  AND r.revenue > 50.00\n" +
	"                        -> ORDER BY r.revenue DESC;\n"

// findInputPreprocessedWarning returns the input_preprocessed warning
// from env.Errors and asserts there is exactly one. Tests that drive
// messySQL through any handler should observe exactly one such warning
// regardless of the tier the handler operates at.
func findInputPreprocessedWarning(t *testing.T, env output.Envelope) output.Error {
	t.Helper()
	var matches []output.Error
	for _, e := range env.Errors {
		if e.Code == output.CodeInputPreprocessed {
			matches = append(matches, e)
		}
	}
	require.Len(t, matches, 1, "expected exactly one input_preprocessed warning, got %d (errors=%+v)", len(matches), env.Errors)
	require.Equal(t, output.SeverityWarning, matches[0].Severity)
	require.Greater(t, matches[0].Context["bytes_removed"], float64(0),
		"bytes_removed must be > 0 when the warning fires")
	return matches[0]
}

// callHandlerWithSQL is a tiny shim that invokes a Tier 1 (sql-only)
// MCP handler with a single SQL argument and returns the unmarshalled
// envelope. Tier 3 tools embed their own DSN argument and use the
// callTier3Handler shim instead.
func callHandlerWithSQL(
	t *testing.T, handler func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error), sql string,
) output.Envelope {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"sql": sql}
	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	return requireEnvelope(t, res)
}

// callTier3Handler invokes a Tier 3 handler with messy SQL and an
// unreachable DSN. The cluster contact will fail, but the
// input_preprocessed warning must still be present in the envelope —
// stripping happens before any network attempt.
func callTier3Handler(
	t *testing.T, handler func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error), sql string,
) output.Envelope {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"sql": sql,
		// Unreachable DSN ensures we never actually contact a cluster
		// while still letting the safety + preprocess plumbing run end
		// to end. connect_timeout=1 keeps the test fast.
		"dsn": "postgres://flaghost:26257/db?connect_timeout=1",
	}
	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	return requireEnvelope(t, res)
}

// TestPreprocessTier1ToolsEmitWarningOnMessyPaste verifies every Tier 1
// (sql-only) MCP handler accepts the issue #161 fixture in one shot
// and surfaces the input_preprocessed warning. The data payload must
// also be populated so the agent gets the actual tool result, not just
// the warning.
func TestPreprocessTier1ToolsEmitWarningOnMessyPaste(t *testing.T) {
	tools := []struct {
		name    string
		handler func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)
	}{
		{name: "parse_sql", handler: ParseSQLHandler(testParserVersion, "")},
		{name: "validate_sql", handler: ValidateSQLHandler(testParserVersion, "")},
		{name: "summarize_sql", handler: SummarizeSQLHandler(testParserVersion, "")},
		{name: "detect_risky_sql", handler: DetectRiskySQLHandler(testParserVersion, "")},
		{name: "format_sql", handler: FormatSQLHandler(testParserVersion, "")},
	}
	for _, tc := range tools {
		t.Run(tc.name, func(t *testing.T) {
			env := callHandlerWithSQL(t, tc.handler, messySQL)
			findInputPreprocessedWarning(t, env)
			require.NotEmpty(t, env.Data, "data payload must be populated on the success path")
			// No ERROR-severity entries: the messy paste must parse
			// after stripping. Warnings (input_preprocessed and any
			// version-mismatch hints) are allowed.
			for _, e := range env.Errors {
				require.NotEqual(t, output.SeverityError, e.Severity,
					"unexpected ERROR-severity entry: %+v", e)
			}
		})
	}
}

// TestPreprocessTier3ToolsEmitWarningOnMessyPaste verifies the
// connected-tier MCP handlers also strip the prompt and surface the
// warning. Cluster contact fails (unreachable DSN) so env.Errors
// includes the connect failure; the assertion is that the
// input_preprocessed warning sits alongside it.
func TestPreprocessTier3ToolsEmitWarningOnMessyPaste(t *testing.T) {
	tools := []struct {
		name    string
		handler func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)
	}{
		{name: "explain_sql", handler: ExplainSQLHandler(testParserVersion, "")},
		{name: "explain_schema_change", handler: ExplainSchemaChangeHandler(testParserVersion, "")},
		{name: "simulate_sql", handler: SimulateSQLHandler(testParserVersion, "")},
		{name: "execute_sql", handler: ExecuteSQLHandler(testParserVersion, "")},
	}
	for _, tc := range tools {
		t.Run(tc.name, func(t *testing.T) {
			env := callTier3Handler(t, tc.handler, messySQL)
			findInputPreprocessedWarning(t, env)
		})
	}
}

// TestPreprocessParseSQLTranslatesPositionToOriginal pins the position-
// translation contract: when the stripped buffer has a parse error,
// the Position attached to the envelope error must point inside the
// original paste (the user's coordinates), not the stripped buffer.
//
// The fixture is a single-line prompt followed by garbage that fails
// to parse. The error sits at the very first non-whitespace token in
// the *stripped* buffer (column 1), but in the *original* it sits one
// character past the prompt prefix (column 28).
func TestPreprocessParseSQLTranslatesPositionToOriginal(t *testing.T) {
	const prompt = "root@localhost:26257/movr> "
	const original = prompt + "GARB AGE\n"

	handler := ParseSQLHandler(testParserVersion, "")
	env := callHandlerWithSQL(t, handler, original)

	// One ERROR (the parse failure) plus one WARNING
	// (input_preprocessed). Order is preprocessSQL first, parse error
	// second.
	findInputPreprocessedWarning(t, env)
	var parseErr *output.Error
	for i := range env.Errors {
		if env.Errors[i].Severity == output.SeverityError {
			parseErr = &env.Errors[i]
			break
		}
	}
	require.NotNil(t, parseErr, "expected a parser error entry in env.Errors: %+v", env.Errors)
	require.NotNil(t, parseErr.Position, "parse error must have a Position so the agent can locate the typo")

	// In the stripped buffer the error sits at line 1 column 1 (the
	// 'G' of "GARB AGE"). In the original buffer that 'G' is at line 1
	// column len(prompt)+1 — that is the value the agent must see.
	require.Equal(t, 1, parseErr.Position.Line)
	require.Equal(t, len(prompt)+1, parseErr.Position.Column,
		"position must be re-derived against the original input, not the stripped buffer")
	require.Equal(t, len(prompt), parseErr.Position.ByteOffset)
}

// TestPreprocessIsNoOpForNonPasteInput pins the hot-path contract: a
// SQL string with no REPL prompts must not trigger the
// input_preprocessed warning. Without this guard, every non-paste
// caller would receive a misleading "input was preprocessed" entry.
func TestPreprocessIsNoOpForNonPasteInput(t *testing.T) {
	handler := ParseSQLHandler(testParserVersion, "")
	env := callHandlerWithSQL(t, handler, "SELECT 1;")

	for _, e := range env.Errors {
		require.NotEqual(t, output.CodeInputPreprocessed, e.Code,
			"input_preprocessed must not fire for prompt-free SQL: %+v", e)
	}
}

// TestPreprocessValidateSQLKeepsCapabilityRequired pins that the
// input_preprocessed warning sits ALONGSIDE other warnings, not
// instead of them. validate_sql with messy SQL and no schemas should
// produce both input_preprocessed (stripping fired) and
// capability_required (name resolution skipped). A regression that
// reverted the append-based envelope handling to assignment would
// drop one of the two — exactly the bug the override-to-append
// migration in the explain/explain_schema_change/simulate handlers
// was meant to prevent.
func TestPreprocessValidateSQLKeepsCapabilityRequired(t *testing.T) {
	handler := ValidateSQLHandler(testParserVersion, "")
	env := callHandlerWithSQL(t, handler, messySQL)

	findInputPreprocessedWarning(t, env)

	var sawCapability bool
	for _, e := range env.Errors {
		if e.Code == "capability_required" {
			sawCapability = true
			break
		}
	}
	require.True(t, sawCapability,
		"capability_required must sit alongside input_preprocessed: %+v", env.Errors)
}

// TestPreprocessTier3PreservesConnectFailure pins that Tier 3 handlers
// surface BOTH the input_preprocessed warning AND the underlying
// connect failure. The override-to-append migration in this PR
// changed `env.Errors = []output.Error{...}` to `append(...)` in
// explain/explain_schema_change/simulate; this test would catch a
// future regression that re-introduced overwrite semantics.
func TestPreprocessTier3PreservesConnectFailure(t *testing.T) {
	tools := []struct {
		name    string
		handler func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)
	}{
		{name: "explain_sql", handler: ExplainSQLHandler(testParserVersion, "")},
		{name: "simulate_sql", handler: SimulateSQLHandler(testParserVersion, "")},
		{name: "execute_sql", handler: ExecuteSQLHandler(testParserVersion, "")},
	}
	for _, tc := range tools {
		t.Run(tc.name, func(t *testing.T) {
			env := callTier3Handler(t, tc.handler, messySQL)
			findInputPreprocessedWarning(t, env)
			require.GreaterOrEqual(t, len(env.Errors), 2,
				"expected input_preprocessed warning and a connect/cluster error: %+v", env.Errors)
			var sawClusterError bool
			for _, e := range env.Errors {
				if e.Code != output.CodeInputPreprocessed && e.Severity == output.SeverityError {
					sawClusterError = true
					break
				}
			}
			require.True(t, sawClusterError,
				"connect failure must remain visible alongside input_preprocessed: %+v", env.Errors)
		})
	}
}
