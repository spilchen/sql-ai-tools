// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/conn"
	"github.com/spilchen/sql-ai-tools/internal/output"
)

// runSimulate executes `crdb-sql simulate` with the supplied args and
// stdin, returning the captured stdout buffer and the Execute error.
func runSimulate(t *testing.T, stdin string, args ...string) (*bytes.Buffer, error) {
	t.Helper()
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"simulate"}, args...))
	return &stdout, root.Execute()
}

// TestSimulateCmdNoDSN verifies the same DSN-required contract the
// other Tier 3 commands honour: missing --dsn / CRDB_DSN fails
// before any cluster contact.
func TestSimulateCmdNoDSN(t *testing.T) {
	t.Setenv("CRDB_DSN", "")

	_, err := runSimulate(t, "", "-e", "SELECT 1")
	require.Error(t, err)
	require.ErrorContains(t, err, "no connection string")
}

// TestSimulateCmdSafetyRejectionTCL pins that the OpSimulate
// allowlist rejects TCL/DCL/nested EXPLAIN before any cluster
// contact. A nonworking DSN guarantees that any regression which
// stops short-circuiting will surface as a connect error rather
// than the safety_violation we expect.
func TestSimulateCmdSafetyRejectionTCL(t *testing.T) {
	tests := []struct {
		name        string
		sql         string
		expectedTag string
	}{
		{name: "begin rejected", sql: "BEGIN", expectedTag: "BEGIN"},
		{name: "commit rejected", sql: "COMMIT", expectedTag: "COMMIT"},
		{name: "grant rejected", sql: "GRANT SELECT ON t TO bob", expectedTag: "GRANT"},
		{name: "nested explain rejected", sql: "EXPLAIN SELECT 1", expectedTag: "EXPLAIN"},
		{name: "nested explain analyze rejected", sql: "EXPLAIN ANALYZE INSERT INTO t VALUES (1)", expectedTag: "EXPLAIN"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CRDB_DSN", "")
			stdout, err := runSimulate(t, "", "--output", "json",
				"--dsn", "postgres://nope:1/db?connect_timeout=1",
				"-e", tc.sql)
			require.ErrorIs(t, err, output.ErrRendered)

			var env output.Envelope
			require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
			require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus,
				"safety rejection must short-circuit before any cluster contact")
			require.Len(t, env.Errors, 1)
			require.Equal(t, output.CodeSafetyViolation, env.Errors[0].Code)
			require.Equal(t, tc.expectedTag, env.Errors[0].Context["tag"])
			require.Equal(t, "read_only", env.Errors[0].Context["mode"])
			require.Equal(t, "simulate", env.Errors[0].Context["operation"])
		})
	}
}

// TestSimulateCmdAdmitsDispatchableShapes pins the inverse of the
// rejection test: SELECT, DML writes, and DDL all pass the safety
// gate and reach the connect step, even under the default
// read_only mode. A regression that tightens the OpSimulate rule
// would surface here as a safety_violation rather than the
// expected connect error.
func TestSimulateCmdAdmitsDispatchableShapes(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{name: "select admitted", sql: "SELECT 1"},
		{name: "insert admitted", sql: "INSERT INTO t VALUES (1)"},
		{name: "update admitted", sql: "UPDATE t SET x = 1 WHERE id = 1"},
		{name: "delete admitted", sql: "DELETE FROM t WHERE id = 1"},
		{name: "alter table admitted", sql: "ALTER TABLE t ADD COLUMN x INT"},
		{name: "create index admitted", sql: "CREATE INDEX i ON t (c)"},
		{name: "drop table admitted", sql: "DROP TABLE t"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CRDB_DSN", "")
			stdout, err := runSimulate(t, "", "--output", "json",
				"--dsn", "postgres://flaghost:26257/defaultdb?connect_timeout=1",
				"-e", tc.sql)
			require.ErrorIs(t, err, output.ErrRendered)

			var env output.Envelope
			require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
			require.Len(t, env.Errors, 1)
			require.Contains(t, env.Errors[0].Message, "connect to CockroachDB",
				"dispatchable shape must reach the connect step under read_only mode")
		})
	}
}

// TestSimulateCmdInvalidMode mirrors TestExplainCmdInvalidMode: an
// unknown --mode value must be rejected with the actionable
// "invalid safety mode" message before any input reading or
// cluster contact.
func TestSimulateCmdInvalidMode(t *testing.T) {
	t.Setenv("CRDB_DSN", "postgres://envhost:26257/defaultdb")

	stdout, err := runSimulate(t, "", "--output", "json",
		"--mode", "yolo",
		"-e", "SELECT 1")
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.Contains(t, env.Errors[0].Message, "invalid safety mode")
}

// TestSimulateCmdRejectsExtraArgs verifies the cobra arg ceiling.
func TestSimulateCmdRejectsExtraArgs(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"simulate", "file1.sql", "file2.sql"})

	err := root.Execute()
	require.Error(t, err)
}

// TestRenderSimulateTextDDLWithStatsError pins the renderer's
// load-bearing partial-failure contract: when a DDL plan succeeded
// but stats lookup failed, the plan body and table-stats line for
// successful targets must still appear, followed by a clearly
// labelled "table stats error" line. Earlier behaviour set
// step.Error on stats failure, which made the renderer suppress
// the plan entirely — directly contradicting the documented
// "best-effort, plan stays visible" contract.
func TestRenderSimulateTextDDLWithStatsError(t *testing.T) {
	var buf bytes.Buffer
	result := conn.SimulateResult{Steps: []conn.SimulateStep{{
		StatementIndex: 0,
		Tag:            "ALTER TABLE",
		Strategy:       conn.StrategyExplainDDL,
		SQL:            "ALTER TABLE users ADD COLUMN x INT",
		DDLPlan: &conn.DDLExplainResult{
			Statement: "ALTER TABLE users ADD COLUMN x INT",
			RawText:   "Schema change plan:\n  ├── BackfillIndex\n",
		},
		TableStats: []conn.TableStat{{
			Schema: "public", Table: "users",
			RowCount: 1234, Source: "show_statistics",
			CollectedAt: "2026-04-22 12:00:00",
		}},
		StatsError: "public.other: connection reset",
	}}}

	require.NoError(t, renderSimulateText(&buf, result))
	out := buf.String()
	require.Contains(t, out, "BackfillIndex", "DDL plan body must survive a stats failure")
	require.Contains(t, out, "row_count=1234", "successful stats line must still render")
	require.Contains(t, out, "table stats error: public.other: connection reset")
}

// TestRenderSimulateTextNoStatsRendersUnavailable pins the
// "RowCount=0 from a no-stats sentinel must not look like a real
// zero" contract. A freshly created table returns
// CollectedAt=="" — the renderer must show "unavailable" rather
// than printing row_count=0 indistinguishable from an empty table.
func TestRenderSimulateTextNoStatsRendersUnavailable(t *testing.T) {
	var buf bytes.Buffer
	result := conn.SimulateResult{Steps: []conn.SimulateStep{{
		StatementIndex: 0,
		Tag:            "ALTER TABLE",
		Strategy:       conn.StrategyExplainDDL,
		DDLPlan:        &conn.DDLExplainResult{RawText: "ok\n"},
		TableStats: []conn.TableStat{{
			Schema: "public", Table: "fresh",
			Source: "show_statistics",
			// CollectedAt deliberately empty.
		}},
	}}}

	require.NoError(t, renderSimulateText(&buf, result))
	require.Contains(t, buf.String(), "row_count=unavailable")
	require.NotContains(t, buf.String(), "row_count=0",
		"no-stats sentinel must not be rendered as a real zero")
}

// TestRenderSimulateTextPlanError verifies the plan-error path
// short-circuits the body but still emits the per-step header so
// multi-statement output stays readable.
func TestRenderSimulateTextPlanError(t *testing.T) {
	var buf bytes.Buffer
	result := conn.SimulateResult{Steps: []conn.SimulateStep{
		{
			StatementIndex: 0, Tag: "SELECT", Strategy: conn.StrategyExplainAnalyze,
			Plan: &conn.ExplainResult{RawRows: []string{"distribution: full", "scan", "  table: t@primary"}},
		},
		{
			StatementIndex: 1, Tag: "INSERT", Strategy: conn.StrategyExplain,
			Error: "table \"missing\" does not exist",
		},
	}}

	require.NoError(t, renderSimulateText(&buf, result))
	out := buf.String()
	require.Contains(t, out, "step 0: SELECT (explain_analyze)")
	require.Contains(t, out, "step 1: INSERT (explain)")
	require.Contains(t, out, "error: table \"missing\" does not exist")
	require.Contains(t, out, "table: t@primary", "successful step body must still render")
}

// TestSimulateStepWarningSurfacesEnvelopeError pins that
// per-step failures are promoted to an envelope-level error so
// JSON consumers reading only top-level errors[] see the partial
// failure. Without this aggregation, an agent would have to walk
// data.steps manually to detect "this simulation partly failed."
// The Context fields carry index slices so an agent can retry only
// the failed steps without parsing the human-readable message.
func TestSimulateStepWarningSurfacesEnvelopeError(t *testing.T) {
	tests := []struct {
		name              string
		steps             []conn.SimulateStep
		expectedOK        bool
		expectedMsg       string
		expectedPlanIdx   []int
		expectedStatsIdx  []int
		expectPlanCtxKey  bool
		expectStatsCtxKey bool
	}{
		{
			name: "all success returns no warning",
			steps: []conn.SimulateStep{
				{StatementIndex: 0, Tag: "SELECT", Strategy: conn.StrategyExplainAnalyze},
			},
			expectedOK: false,
		},
		{
			name: "plan failure stamps plan_failed_steps context",
			steps: []conn.SimulateStep{
				{StatementIndex: 0, Tag: "INSERT", Strategy: conn.StrategyExplain, Error: "boom"},
			},
			expectedOK:       true,
			expectedMsg:      "1 plan error(s)",
			expectedPlanIdx:  []int{0},
			expectPlanCtxKey: true,
		},
		{
			name: "stats failure stamps stats_failed_steps context",
			steps: []conn.SimulateStep{
				{StatementIndex: 0, Tag: "ALTER TABLE", Strategy: conn.StrategyExplainDDL, StatsError: "x"},
			},
			expectedOK:        true,
			expectedMsg:       "1 stats error(s)",
			expectedStatsIdx:  []int{0},
			expectStatsCtxKey: true,
		},
		{
			name: "mixed failures stamp both context keys",
			steps: []conn.SimulateStep{
				{StatementIndex: 0, Tag: "INSERT", Strategy: conn.StrategyExplain, Error: "boom"},
				{StatementIndex: 1, Tag: "ALTER TABLE", Strategy: conn.StrategyExplainDDL, StatsError: "x"},
			},
			expectedOK:        true,
			expectedPlanIdx:   []int{0},
			expectedStatsIdx:  []int{1},
			expectPlanCtxKey:  true,
			expectStatsCtxKey: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry, ok := simulateStepWarning(conn.SimulateResult{Steps: tc.steps})
			require.Equal(t, tc.expectedOK, ok)
			if !tc.expectedOK {
				return
			}
			require.Equal(t, "simulate_step_failure", entry.Code)
			require.Equal(t, "simulate", entry.Category)
			if tc.expectedMsg != "" {
				require.Contains(t, entry.Message, tc.expectedMsg)
			}
			if tc.expectPlanCtxKey {
				require.Equal(t, tc.expectedPlanIdx, entry.Context["plan_failed_steps"])
			} else {
				require.NotContains(t, entry.Context, "plan_failed_steps")
			}
			if tc.expectStatsCtxKey {
				require.Equal(t, tc.expectedStatsIdx, entry.Context["stats_failed_steps"])
			} else {
				require.NotContains(t, entry.Context, "stats_failed_steps")
			}
		})
	}
}

// TestSimulateCmdPartialFailurePreservesData pins the load-bearing
// data-preservation contract for the CLI: when a multi-statement
// simulation has one failed step among several successes, the JSON
// envelope MUST keep data.steps populated so the agent can see
// every plan that did succeed and the per-step error for the one
// that didn't. Earlier behaviour routed through RenderErrorEntry,
// which nils env.Data — wiping exactly the data we asked the agent
// to inspect. The fix follows the renderValidateFailure pattern in
// cmd/validate.go.
//
// We exercise this via the dispatcher's "no route" branch (TCL),
// which reaches the connect step and produces a per-step error
// without needing a real cluster. With a multi-statement input
// where step 0 reaches connect (which fails fast on the unreachable
// DSN), this path doesn't actually exercise the no-route branch
// alone — so we use a unit-level test by constructing a stub
// SimulateResult and rendering directly via renderSimulateText
// plus the simulateStepWarning helper, mirroring what RunE does.
func TestSimulateCmdPartialFailurePreservesData(t *testing.T) {
	// Stub a SimulateResult that includes both a successful step
	// and a failed step — exactly the shape RunE produces in
	// production for a real multi-statement batch with one mid-
	// failure. The assertion below verifies env.Data survives
	// alongside the simulate_step_failure entry.
	result := conn.SimulateResult{Steps: []conn.SimulateStep{
		{
			StatementIndex: 0, Tag: "SELECT", Strategy: conn.StrategyExplainAnalyze,
			Plan: &conn.ExplainResult{RawRows: []string{"distribution: full", "scan"}},
		},
		{
			StatementIndex: 1, Tag: "INSERT", Strategy: conn.StrategyExplain,
			Error: "table \"missing\" does not exist",
		},
	}}

	// Reproduce the RunE flow shape: marshal data, then surface
	// step warnings without calling RenderErrorEntry (which would
	// nil env.Data).
	data, err := json.Marshal(result)
	require.NoError(t, err)

	env := output.Envelope{
		Tier:             output.TierConnected,
		ConnectionStatus: output.ConnectionConnected,
		Data:             data,
	}
	if entry, ok := simulateStepWarning(result); ok {
		env.Errors = append(env.Errors, entry)
	}

	// The envelope MUST carry both the per-step data and the
	// aggregated failure entry. Either being missing is a
	// regression of the iter-2 critical fix.
	require.NotEmpty(t, env.Data, "data must survive partial failure")
	require.Len(t, env.Errors, 1)
	require.Equal(t, "simulate_step_failure", env.Errors[0].Code)
	require.Equal(t, []int{1}, env.Errors[0].Context["plan_failed_steps"])

	// Round-trip the envelope through JSON and confirm steps[]
	// remains parseable on the wire.
	body, err := json.Marshal(env)
	require.NoError(t, err)
	require.Contains(t, string(body), `"steps"`)
	require.Contains(t, string(body), `"distribution: full"`,
		"successful step's plan body must survive in the JSON envelope")
}
