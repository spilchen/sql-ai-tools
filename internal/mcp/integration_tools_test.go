// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Per-tool integration assertions for the MCP stdio server. Helpers
// and lifecycle live in integration_test.go. Each top-level test
// runs against a fresh subprocess so a hung handler or panic in one
// test does not cascade.

package mcp_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	internalmcp "github.com/spilchen/sql-ai-tools/internal/mcp"
	"github.com/spilchen/sql-ai-tools/internal/mcp/tools"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/testutil/cockroachtest"
	"github.com/spilchen/sql-ai-tools/internal/validateresult"
)

// expectedTools enumerates every tool the server registers in
// internal/mcp/server.go. Kept here (rather than imported from the
// server) so the registration set must be updated in two places — a
// removed s.AddTool() line fails this test loudly, and a newly added
// tool that someone forgets to list here also fails, forcing the
// list to stay in sync with reality.
var expectedTools = []string{
	internalmcp.PingToolName,
	tools.ParseSQLToolName,
	tools.ValidateSQLToolName,
	tools.FormatSQLToolName,
	tools.DetectRiskyQueryToolName,
	tools.SummarizeSQLToolName,
	tools.ExplainSQLToolName,
	tools.ExplainSchemaChangeToolName,
	tools.ListTablesToolName,
	tools.DescribeTableToolName,
}

// usersSchema is the inline CREATE TABLE used by the catalog-based
// tools (list_tables, describe_table, validate_sql with schemas).
const usersSchema = `CREATE TABLE users (id INT PRIMARY KEY, name STRING NOT NULL);`

// TestIntegrationToolsListed locks in the registered tool surface.
// Adding or removing a tool in internal/mcp/server.go without
// updating expectedTools fails this test.
func TestIntegrationToolsListed(t *testing.T) {
	c := newMCPClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()

	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	require.NoError(t, err)

	got := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
	}
	require.ElementsMatch(t, expectedTools, got)
}

// TestIntegrationPing covers the health-check tool. ping is the only
// tool that does not return an output.Envelope — it returns
// {ok, parser_version} directly — so it gets its own decode path.
func TestIntegrationPing(t *testing.T) {
	c := newMCPClient(t)
	res := callTool(t, c, internalmcp.PingToolName, nil)
	require.False(t, res.IsError, "ping returned tool error: %s", textOf(res))

	require.Len(t, res.Content, 1)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)

	var payload struct {
		OK            bool   `json:"ok"`
		ParserVersion string `json:"parser_version"`
	}
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &payload))
	require.True(t, payload.OK)
	require.NotEmpty(t, payload.ParserVersion, "parser_version must be stamped")
}

// TestIntegrationParseSQL covers the success and parse-error paths
// for parse_sql. The success case asserts the envelope's data carries
// at least one classified statement; the error case asserts a 42601
// SQLSTATE with position lands in env.Errors while the tool itself
// still returns IsError=false. (Tool-level errors — missing required
// params — surface as IsError=true; see TestIntegrationValidateSQL.)
func TestIntegrationParseSQL(t *testing.T) {
	c := newMCPClient(t)

	tests := []struct {
		name              string
		sql               string
		expectedErrCode   string // empty means success path
		expectedFirstTag  string // only checked on success path
		expectedFirstType string // only checked on success path
	}{
		{
			name:              "valid select",
			sql:               "SELECT 1",
			expectedFirstTag:  "SELECT",
			expectedFirstType: "DML",
		},
		{
			name:            "syntax error",
			sql:             "SELEC * FROM t",
			expectedErrCode: "42601",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := callTool(t, c, tools.ParseSQLToolName, map[string]any{"sql": tc.sql})
			env := decodeEnvelope(t, res)
			assertTier1Envelope(t, env)

			if tc.expectedErrCode != "" {
				assertEnvelopeError(t, env, tc.expectedErrCode)
				return
			}

			require.Empty(t, env.Errors, "success path must have no errors")
			require.NotEmpty(t, env.Data, "success path must populate data")
			var stmts []struct {
				StatementType string `json:"statement_type"`
				Tag           string `json:"tag"`
			}
			require.NoError(t, json.Unmarshal(env.Data, &stmts))
			require.NotEmpty(t, stmts)
			require.Equal(t, tc.expectedFirstTag, stmts[0].Tag)
			require.Equal(t, tc.expectedFirstType, stmts[0].StatementType)
		})
	}
}

// TestIntegrationValidateSQL covers the dual-tier behavior of
// validate_sql plus its tool-level error path. Tier 1 (no schemas)
// returns a capability_required warning; Tier 2 (with schemas)
// reports name_resolution=ok. A type error is reported as an
// envelope error; a missing required parameter is a tool-level error
// (IsError=true) per the discipline in internal/mcp/tools/tools.go.
func TestIntegrationValidateSQL(t *testing.T) {
	c := newMCPClient(t)

	t.Run("tier 1 zero config skips name resolution", func(t *testing.T) {
		res := callTool(t, c, tools.ValidateSQLToolName, map[string]any{"sql": "SELECT 1"})
		env := decodeEnvelope(t, res)
		require.Equal(t, output.TierZeroConfig, env.Tier)
		require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)

		// The capability_required warning is the only envelope entry on
		// the zero-config success path.
		require.Len(t, env.Errors, 1)
		require.Equal(t, validateresult.CapabilityRequiredCode, env.Errors[0].Code)
		require.Equal(t, output.SeverityWarning, env.Errors[0].Severity)

		var result validateresult.Result
		require.NoError(t, json.Unmarshal(env.Data, &result))
		require.True(t, result.Valid)
		require.Equal(t, validateresult.CheckOK, result.Checks.Syntax)
		require.Equal(t, validateresult.CheckOK, result.Checks.TypeCheck)
		require.Equal(t, validateresult.CheckSkipped, result.Checks.NameResolution)
	})

	t.Run("tier 2 with schemas runs name resolution", func(t *testing.T) {
		res := callTool(t, c, tools.ValidateSQLToolName, map[string]any{
			"sql":     "SELECT id FROM users",
			"schemas": []any{map[string]any{"sql": usersSchema}},
		})
		env := decodeEnvelope(t, res)
		require.Equal(t, output.TierSchemaFile, env.Tier)
		require.Empty(t, env.Errors, "clean validate must have no envelope errors")

		var result validateresult.Result
		require.NoError(t, json.Unmarshal(env.Data, &result))
		require.True(t, result.Valid)
		require.Equal(t, validateresult.CheckOK, result.Checks.NameResolution)
	})

	t.Run("type mismatch surfaces as envelope error", func(t *testing.T) {
		res := callTool(t, c, tools.ValidateSQLToolName, map[string]any{"sql": "SELECT 1 + 'abc'"})
		env := decodeEnvelope(t, res)
		require.Equal(t, output.TierZeroConfig, env.Tier)
		// The Tier 0 path also emits a capability_required warning
		// when schemas are absent, so the type error is no longer
		// guaranteed to be at index 0. Find it by severity.
		var typeErr *output.Error
		for i := range env.Errors {
			if env.Errors[i].Severity == output.SeverityError {
				typeErr = &env.Errors[i]
				break
			}
		}
		require.NotNil(t, typeErr, "type-check failure must populate envelope errors")
		// Type errors carry a SQLSTATE from the parser/type checker;
		// avoid pinning the exact code so a parser bump that refines
		// the diagnosis (e.g. 42883 → 42804) does not break the test.
		require.NotEmpty(t, typeErr.Code)
	})

	t.Run("missing sql parameter is tool error", func(t *testing.T) {
		res := callTool(t, c, tools.ValidateSQLToolName, map[string]any{})
		require.True(t, res.IsError, "missing required param must surface as tool error, got: %s", textOf(res))
	})

	// The next two cases cover the wiring added in #15: column errors
	// reach the MCP envelope (closing a pre-existing gap where only
	// CheckTableNames ran in this handler) and carry structured
	// "did you mean?" suggestions.
	t.Run("column typo surfaces suggestion", func(t *testing.T) {
		res := callTool(t, c, tools.ValidateSQLToolName, map[string]any{
			"sql":     "SELECT nme FROM users",
			"schemas": []any{map[string]any{"sql": usersSchema}},
		})
		env := decodeEnvelope(t, res)
		require.Len(t, env.Errors, 1)
		require.Equal(t, "42703", env.Errors[0].Code)
		require.NotEmpty(t, env.Errors[0].Suggestions)
		require.Equal(t, "name", env.Errors[0].Suggestions[0].Replacement)
	})

	t.Run("table typo surfaces suggestion", func(t *testing.T) {
		res := callTool(t, c, tools.ValidateSQLToolName, map[string]any{
			"sql":     "SELECT * FROM usrs",
			"schemas": []any{map[string]any{"sql": usersSchema}},
		})
		env := decodeEnvelope(t, res)
		require.Len(t, env.Errors, 1)
		require.Equal(t, "42P01", env.Errors[0].Code)
		require.NotEmpty(t, env.Errors[0].Suggestions)
		require.Equal(t, "users", env.Errors[0].Suggestions[0].Replacement)
	})

	// Regression guard: lock in that CheckColumnNames runs in the MCP
	// handler at all, independent of whether a suggestion is produced.
	// A future refactor that removes the CheckColumnNames call would
	// make the suggestion-bearing tests above ambiguous (the column
	// error might still leak through some other path), so a wholly
	// unrelated column name — for which Suggest will return no
	// candidates — locks the wiring on its own.
	t.Run("unrelated column name still produces 42703", func(t *testing.T) {
		res := callTool(t, c, tools.ValidateSQLToolName, map[string]any{
			"sql":     "SELECT completely_unrelated_xyzzy FROM users",
			"schemas": []any{map[string]any{"sql": usersSchema}},
		})
		env := decodeEnvelope(t, res)
		require.Len(t, env.Errors, 1)
		require.Equal(t, "42703", env.Errors[0].Code)
		require.Empty(t, env.Errors[0].Suggestions, "no candidate is close enough to suggest")
	})
}

// TestIntegrationFormatSQL covers pretty-printing on the happy path
// and parse-error reporting. The success case asserts the data
// payload exposes formatted_sql; the error case asserts a 42601 with
// position.
func TestIntegrationFormatSQL(t *testing.T) {
	c := newMCPClient(t)

	t.Run("formats valid select", func(t *testing.T) {
		res := callTool(t, c, tools.FormatSQLToolName, map[string]any{"sql": "select   1"})
		env := decodeEnvelope(t, res)
		assertTier1Envelope(t, env)
		require.Empty(t, env.Errors)

		var data struct {
			FormattedSQL string `json:"formatted_sql"`
		}
		require.NoError(t, json.Unmarshal(env.Data, &data))
		require.NotEmpty(t, data.FormattedSQL)
		// The formatter normalizes whitespace, so the doubled space
		// from the input must not survive into the output.
		require.NotContains(t, data.FormattedSQL, "select   1")
	})

	t.Run("syntax error in envelope", func(t *testing.T) {
		res := callTool(t, c, tools.FormatSQLToolName, map[string]any{"sql": "SELEC * FROM t"})
		env := decodeEnvelope(t, res)
		assertTier1Envelope(t, env)
		assertEnvelopeError(t, env, "42601")
	})
}

// TestIntegrationDetectRiskyQuery asserts the tool returns a non-empty
// findings array for an obvious risky pattern (DROP TABLE) and an
// empty array for a clean SELECT. Specific rule codes are intentionally
// not pinned — the rule registry is expected to grow.
func TestIntegrationDetectRiskyQuery(t *testing.T) {
	c := newMCPClient(t)

	tests := []struct {
		name             string
		sql              string
		expectedFindings bool
	}{
		{name: "clean select has no findings", sql: "SELECT id FROM users WHERE id = 1", expectedFindings: false},
		{name: "drop table flagged as risky", sql: "DROP TABLE users", expectedFindings: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := callTool(t, c, tools.DetectRiskyQueryToolName, map[string]any{"sql": tc.sql})
			env := decodeEnvelope(t, res)
			assertTier1Envelope(t, env)
			require.Empty(t, env.Errors)

			var findings []map[string]any
			require.NoError(t, json.Unmarshal(env.Data, &findings))
			if tc.expectedFindings {
				require.NotEmpty(t, findings, "expected at least one risky-pattern finding")
			} else {
				require.Empty(t, findings)
			}
		})
	}
}

// TestIntegrationSummarizeSQL asserts the tool returns one Summary per
// statement with the expected operation and table set. Specific field
// shapes (predicates, joins, affected_columns) are pinned in the
// summarize package's own tests; this test exists to catch wiring
// breakage between server registration, handler dispatch, and JSON
// round-trip across the stdio boundary.
func TestIntegrationSummarizeSQL(t *testing.T) {
	c := newMCPClient(t)

	res := callTool(t, c, tools.SummarizeSQLToolName, map[string]any{
		"sql": "DELETE FROM orders WHERE status='x'",
	})
	env := decodeEnvelope(t, res)
	assertTier1Envelope(t, env)
	require.Empty(t, env.Errors)

	var summaries []map[string]any
	require.NoError(t, json.Unmarshal(env.Data, &summaries))
	require.Len(t, summaries, 1)
	require.Equal(t, "DELETE", summaries[0]["operation"])
	require.Equal(t, []any{"orders"}, summaries[0]["tables"])
}

// TestIntegrationListTables covers list_tables on both the happy path
// (schemas yield a non-empty tables array) and the missing-required-
// parameter path (tool error). schemas is required for list_tables so
// omitting it is an infrastructure error, not an envelope error.
func TestIntegrationListTables(t *testing.T) {
	c := newMCPClient(t)

	t.Run("inline schemas yield tables", func(t *testing.T) {
		res := callTool(t, c, tools.ListTablesToolName, map[string]any{
			"schemas": []any{map[string]any{"sql": usersSchema}},
		})
		env := decodeEnvelope(t, res)
		require.Equal(t, output.TierSchemaFile, env.Tier)
		require.Empty(t, env.Errors)

		var data struct {
			Tables []string `json:"tables"`
		}
		require.NoError(t, json.Unmarshal(env.Data, &data))
		require.Contains(t, data.Tables, "users")
	})

	t.Run("missing schemas is tool error", func(t *testing.T) {
		res := callTool(t, c, tools.ListTablesToolName, map[string]any{})
		require.True(t, res.IsError, "missing required schemas must surface as tool error, got: %s", textOf(res))
	})
}

// TestIntegrationDescribeTable covers the catalog hit/miss split.
// A hit returns the table struct in env.Data; a miss returns a
// structured 42P01 envelope error whose Context.available_tables
// names the loaded tables, so agents can suggest a correction. The
// miss subtest verifies the loaded "users" table appears in that
// list — an empty or wrong-shaped value would silently pass a
// key-only check.
func TestIntegrationDescribeTable(t *testing.T) {
	c := newMCPClient(t)
	schemas := []any{map[string]any{"sql": usersSchema}}

	t.Run("known table returns description", func(t *testing.T) {
		res := callTool(t, c, tools.DescribeTableToolName, map[string]any{
			"table":   "users",
			"schemas": schemas,
		})
		env := decodeEnvelope(t, res)
		require.Equal(t, output.TierSchemaFile, env.Tier)
		require.Empty(t, env.Errors)

		var tbl struct {
			Name    string           `json:"name"`
			Columns []map[string]any `json:"columns"`
		}
		require.NoError(t, json.Unmarshal(env.Data, &tbl))
		require.Equal(t, "users", tbl.Name)
		require.NotEmpty(t, tbl.Columns)
	})

	t.Run("unknown table is 42P01 envelope error", func(t *testing.T) {
		res := callTool(t, c, tools.DescribeTableToolName, map[string]any{
			"table":   "missing",
			"schemas": schemas,
		})
		env := decodeEnvelope(t, res)
		require.Equal(t, output.TierSchemaFile, env.Tier)
		require.Len(t, env.Errors, 1)
		require.Equal(t, "42P01", env.Errors[0].Code)
		require.Equal(t, output.SeverityError, env.Errors[0].Severity)
		// JSON arrays come back as []any; the value must include the
		// actual loaded table so a correction-suggestion path can
		// render it. A key-only assertion would let an empty slice
		// (catalog-population bug) silently pass.
		avail, ok := env.Errors[0].Context["available_tables"].([]any)
		require.True(t, ok, "available_tables must be a JSON array")
		require.Contains(t, avail, "users")
	})
}

// TestIntegrationExplainSQL exercises the only Tier 3 tool. It is
// gated on cockroachtest.Shared, which skips cleanly when neither
// COCKROACH_BIN nor CRDB_TEST_DSN is set, so this test runs as part
// of `make test-integration` but is a clean skip under `make test`.
func TestIntegrationExplainSQL(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	c := newMCPClient(t)

	// EXPLAIN can take longer than the default callTimeout on a cold
	// cluster, so issue this call directly with a generous timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var req mcp.CallToolRequest
	req.Params.Name = tools.ExplainSQLToolName
	req.Params.Arguments = map[string]any{
		"sql": "SELECT 1",
		"dsn": cluster.DSN,
	}
	res, err := c.CallTool(ctx, req)
	require.NoError(t, err)

	env := decodeEnvelope(t, res)
	require.Equal(t, output.TierConnected, env.Tier)
	require.Equal(t, output.ConnectionConnected, env.ConnectionStatus)
	require.Empty(t, env.Errors, "EXPLAIN of SELECT 1 must succeed")
	require.NotEmpty(t, env.Data, "EXPLAIN must populate data")
}

// assertTier1Envelope checks the two invariants every Tier 1 tool
// upholds: tier=zero_config and connection_status=disconnected.
// ParserVersion is also asserted non-empty so a regression that drops
// the version stamping fails loudly.
func assertTier1Envelope(t *testing.T, env output.Envelope) {
	t.Helper()
	require.Equal(t, output.TierZeroConfig, env.Tier)
	require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)
	require.NotEmpty(t, env.ParserVersion)
}

// assertEnvelopeError asserts the envelope reports at least one error
// with the given SQLSTATE code, ERROR severity, and a non-nil
// Position. Position is unconditional because every caller is a
// parser-error path — and parser errors that don't know where they
// failed are themselves a regression.
func assertEnvelopeError(t *testing.T, env output.Envelope, code string) {
	t.Helper()
	require.NotEmpty(t, env.Errors, "expected an envelope error")
	first := env.Errors[0]
	require.Equal(t, code, first.Code)
	require.Equal(t, output.SeverityError, first.Severity)
	require.NotNil(t, first.Position, "parser errors must carry source position")
}
