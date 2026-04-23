// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package conn

import (
	"testing"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/stretchr/testify/require"
)

// TestSimulateStrategyForAST pins the dispatcher's routing table.
// Since the routing decision determines whether the inner statement
// executes (StrategyExplainAnalyze) or not (StrategyExplain /
// StrategyExplainDDL), a regression here would be the difference
// between "simulated" and "actually wrote data" — exactly the
// safety property the EXPLAIN-based dispatcher exists to provide.
func TestSimulateStrategyForAST(t *testing.T) {
	tests := []struct {
		name             string
		sql              string
		expectedStrategy Strategy
		expectedRouted   bool
	}{
		{
			name:             "select goes to explain analyze",
			sql:              "SELECT * FROM t",
			expectedStrategy: StrategyExplainAnalyze,
			expectedRouted:   true,
		},
		{
			name:             "values goes to explain analyze",
			sql:              "VALUES (1), (2)",
			expectedStrategy: StrategyExplainAnalyze,
			expectedRouted:   true,
		},
		{
			name:             "with cte select goes to explain analyze",
			sql:              "WITH cte AS (SELECT 1) SELECT * FROM cte",
			expectedStrategy: StrategyExplainAnalyze,
			expectedRouted:   true,
		},
		{
			name:             "insert goes to plain explain",
			sql:              "INSERT INTO t VALUES (1)",
			expectedStrategy: StrategyExplain,
			expectedRouted:   true,
		},
		{
			name:             "update goes to plain explain",
			sql:              "UPDATE t SET x = 1 WHERE id = 1",
			expectedStrategy: StrategyExplain,
			expectedRouted:   true,
		},
		{
			name:             "delete goes to plain explain",
			sql:              "DELETE FROM t WHERE id = 1",
			expectedStrategy: StrategyExplain,
			expectedRouted:   true,
		},
		{
			name:             "upsert goes to plain explain",
			sql:              "UPSERT INTO t VALUES (1)",
			expectedStrategy: StrategyExplain,
			expectedRouted:   true,
		},
		{
			name:             "alter table goes to explain ddl",
			sql:              "ALTER TABLE t ADD COLUMN x INT",
			expectedStrategy: StrategyExplainDDL,
			expectedRouted:   true,
		},
		{
			name:             "create index goes to explain ddl",
			sql:              "CREATE INDEX i ON t (c)",
			expectedStrategy: StrategyExplainDDL,
			expectedRouted:   true,
		},
		{
			name:             "drop table goes to explain ddl",
			sql:              "DROP TABLE t",
			expectedStrategy: StrategyExplainDDL,
			expectedRouted:   true,
		},
		{
			name:           "begin has no route",
			sql:            "BEGIN",
			expectedRouted: false,
		},
		{
			name:           "commit has no route",
			sql:            "COMMIT",
			expectedRouted: false,
		},
		{
			name:           "grant has no route",
			sql:            "GRANT SELECT ON t TO bob",
			expectedRouted: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stmts, err := parser.Parse(tc.sql)
			require.NoError(t, err)
			require.Len(t, stmts, 1)

			strategy, routed := simulateStrategyForAST(stmts[0].AST)
			require.Equal(t, tc.expectedRouted, routed)
			if tc.expectedRouted {
				require.Equal(t, tc.expectedStrategy, strategy)
			}
		})
	}
}

// TestDDLTargets pins the AST-target extraction used to drive the
// SHOW STATISTICS lookup. The dispatcher relies on this to label
// per-step TableStats, so a missed extraction would silently drop
// the row-count annotation rather than fail loudly.
func TestDDLTargets(t *testing.T) {
	tests := []struct {
		name            string
		sql             string
		expectedTargets []ddlTarget
	}{
		{
			name:            "alter table unqualified",
			sql:             "ALTER TABLE users ADD COLUMN x INT",
			expectedTargets: []ddlTarget{{Schema: "", Table: "users"}},
		},
		{
			name:            "alter table qualified",
			sql:             "ALTER TABLE public.users ADD COLUMN x INT",
			expectedTargets: []ddlTarget{{Schema: "public", Table: "users"}},
		},
		{
			name:            "create index qualified",
			sql:             "CREATE INDEX i ON public.users (email)",
			expectedTargets: []ddlTarget{{Schema: "public", Table: "users"}},
		},
		{
			name: "drop table multiple",
			sql:  "DROP TABLE public.a, public.b",
			expectedTargets: []ddlTarget{
				{Schema: "public", Table: "a"},
				{Schema: "public", Table: "b"},
			},
		},
		{
			name:            "drop index single",
			sql:             "DROP INDEX public.users@users_email_idx",
			expectedTargets: []ddlTarget{{Schema: "public", Table: "users"}},
		},
		{
			name:            "create table has no extractable target",
			sql:             "CREATE TABLE x (id INT PRIMARY KEY)",
			expectedTargets: nil,
		},
		{
			name:            "create schema has no extractable target",
			sql:             "CREATE SCHEMA app",
			expectedTargets: nil,
		},
		{
			name:            "select is not a DDL target",
			sql:             "SELECT 1",
			expectedTargets: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stmts, err := parser.Parse(tc.sql)
			require.NoError(t, err)
			require.Len(t, stmts, 1)

			targets := ddlTargets(stmts[0].AST)
			require.Equal(t, tc.expectedTargets, targets)
		})
	}
}

// TestSimulateRejectsBadInput pins the input-validation surface of
// Simulate that runs before any cluster contact. A parse failure or
// empty input must return without dialing — otherwise a malformed
// agent request would burn a connection per call.
func TestSimulateRejectsBadInput(t *testing.T) {
	mgr := NewManager("postgres://localhost:1/defaultdb")
	t.Cleanup(func() { _ = mgr.Close(t.Context()) })

	tests := []struct {
		name        string
		sql         string
		expectedErr string
	}{
		{
			name:        "parse error surfaces with prefix",
			sql:         "SELEKT broken",
			expectedErr: "parse simulate input",
		},
		{
			name:        "empty input rejected",
			sql:         "",
			expectedErr: "no statements parsed",
		},
		{
			name:        "whitespace only rejected",
			sql:         "   \n\t",
			expectedErr: "no statements parsed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mgr.Simulate(t.Context(), tc.sql)
			require.ErrorContains(t, err, tc.expectedErr)
			require.Nil(t, mgr.conn, "Simulate must not dial on input rejection")
		})
	}
}

// TestStrategyConstantsAreStable pins the wire values for the
// Strategy enum. Agents branch on these tokens to decide which
// payload field to read; a rename here would silently break every
// downstream consumer.
func TestStrategyConstantsAreStable(t *testing.T) {
	require.Equal(t, Strategy("explain_analyze"), StrategyExplainAnalyze)
	require.Equal(t, Strategy("explain"), StrategyExplain)
	require.Equal(t, Strategy("explain_ddl"), StrategyExplainDDL)
}

// TestStepFailureSummary pins the per-step failure aggregation that
// drives the CLI's exit code and the MCP envelope's Errors entry.
// Without this aggregation, a multi-statement simulation where one
// step failed would render as fully successful to any consumer
// reading only the top-level errors[] array.
func TestStepFailureSummary(t *testing.T) {
	tests := []struct {
		name             string
		steps            []SimulateStep
		expectedOK       bool
		expectedMsgPart  string
		expectedPlanIdx  []int
		expectedStatsIdx []int
	}{
		{
			name: "all success",
			steps: []SimulateStep{
				{StatementIndex: 0, Tag: "SELECT", Strategy: StrategyExplainAnalyze},
				{StatementIndex: 1, Tag: "INSERT", Strategy: StrategyExplain},
			},
			expectedOK: false,
		},
		{
			name: "single plan failure",
			steps: []SimulateStep{
				{StatementIndex: 0, Tag: "SELECT", Strategy: StrategyExplainAnalyze, Error: "boom"},
			},
			expectedOK:      true,
			expectedMsgPart: "1 plan error(s) at step(s) [0]",
			expectedPlanIdx: []int{0},
		},
		{
			name: "stats-only failure preserves the message split",
			steps: []SimulateStep{
				{StatementIndex: 0, Tag: "ALTER TABLE", Strategy: StrategyExplainDDL, StatsError: "users.x: nope"},
			},
			expectedOK:       true,
			expectedMsgPart:  "1 stats error(s) at step(s) [0]",
			expectedStatsIdx: []int{0},
		},
		{
			name: "mixed plan and stats failures across statements",
			steps: []SimulateStep{
				{StatementIndex: 0, Tag: "SELECT", Strategy: StrategyExplainAnalyze},
				{StatementIndex: 1, Tag: "INSERT", Strategy: StrategyExplain, Error: "boom"},
				{StatementIndex: 2, Tag: "ALTER TABLE", Strategy: StrategyExplainDDL, StatsError: "lookup failed"},
			},
			expectedOK:       true,
			expectedPlanIdx:  []int{1},
			expectedStatsIdx: []int{2},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := SimulateResult{Steps: tc.steps}
			msg, planFails, statsFails, ok := result.StepFailureSummary()
			require.Equal(t, tc.expectedOK, ok)
			if !tc.expectedOK {
				require.Empty(t, msg)
				require.Empty(t, planFails)
				require.Empty(t, statsFails)
				return
			}
			require.Equal(t, tc.expectedPlanIdx, planFails)
			require.Equal(t, tc.expectedStatsIdx, statsFails)
			if tc.expectedMsgPart != "" {
				require.Contains(t, msg, tc.expectedMsgPart)
			}
			require.Contains(t, msg, "see data.steps")
		})
	}
}

// TestDispatchSimulateStepNoRoute pins the defense-in-depth
// behaviour for statement classes the safety gate is supposed to
// reject upstream (TCL, DCL). If a future caller bypasses
// safety.Check, the dispatcher's default arm must record an
// actionable per-step error rather than panicking or silently
// routing. We exercise dispatchSimulateStep directly with a nil
// connection because the no-route branch never touches the conn.
func TestDispatchSimulateStepNoRoute(t *testing.T) {
	tests := []struct {
		name        string
		sql         string
		expectedTag string
	}{
		{name: "TCL begin has no route", sql: "BEGIN", expectedTag: "BEGIN"},
		{name: "TCL commit has no route", sql: "COMMIT", expectedTag: "COMMIT"},
		{name: "DCL grant has no route", sql: "GRANT SELECT ON t TO bob", expectedTag: "GRANT"},
		{name: "DCL revoke has no route", sql: "REVOKE SELECT ON t FROM bob", expectedTag: "REVOKE"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stmts, err := parser.Parse(tc.sql)
			require.NoError(t, err)
			require.Len(t, stmts, 1)

			mgr := NewManager("postgres://localhost:1/defaultdb")
			t.Cleanup(func() { _ = mgr.Close(t.Context()) })

			step := SimulateStep{
				StatementIndex: 0,
				Tag:            stmts[0].AST.StatementTag(),
				SQL:            stmts[0].SQL,
			}
			mgr.dispatchSimulateStep(t.Context(), stmts[0].AST, stmts[0].SQL, &step)

			require.Equal(t, tc.expectedTag, step.Tag)
			require.Empty(t, step.Strategy, "no route means no strategy was selected")
			require.Nil(t, step.Plan)
			require.Nil(t, step.DDLPlan)
			require.Empty(t, step.StatsError)
			require.Contains(t, step.Error, "no route for statement type")
			require.Contains(t, step.Error, tc.expectedTag)
		})
	}
}

// TestCollectDDLTableStatsAggregatesEveryFailure pins the
// "every per-target error class survives" contract. Earlier
// behaviour kept only the first failure message ("first error
// wins"), which silently dropped distinct error classes from later
// targets — e.g. a permission-denied on table `a` would hide a
// connection-refused on table `b`. The fix joins every per-target
// error with `; ` so an operator debugging the simulation sees
// each one.
//
// We exercise this with `DROP TABLE a, b` against a Manager whose
// connect step is guaranteed to fail (unreachable DSN). Each
// per-target GetTableStats call produces the same connect error,
// so the joined StatsError must mention both `a` and `b`. A
// regression that reverts to "first error wins" would fail the
// `b` assertion.
func TestCollectDDLTableStatsAggregatesEveryFailure(t *testing.T) {
	mgr := NewManager("postgres://nope:1/db?connect_timeout=1")
	t.Cleanup(func() { _ = mgr.Close(t.Context()) })

	stmts, err := parser.Parse("DROP TABLE public.a, public.b")
	require.NoError(t, err)
	require.Len(t, stmts, 1)

	stats, statsErr := mgr.collectDDLTableStats(t.Context(), stmts[0].AST)
	require.Empty(t, stats, "every per-target lookup should fail against an unreachable DSN")
	require.NotEmpty(t, statsErr)
	require.Contains(t, statsErr, "public.a")
	require.Contains(t, statsErr, "public.b",
		"second target's failure must not be dropped — agents need every error class")
	require.Contains(t, statsErr, ";",
		"per-target failures must be joined with `; ` separators")
}

// TestRunGetTableStatsRejectsEmptyTable pins the load-bearing
// validation in runGetTableStats that runs before any cluster
// contact. An empty table name would let pgx.Identifier.Sanitize
// produce a literal empty-quoted identifier, which the cluster
// would reject with a confusing message; rejecting here keeps the
// error message actionable.
func TestRunGetTableStatsRejectsEmptyTable(t *testing.T) {
	mgr := NewManager("postgres://localhost:1/defaultdb")
	t.Cleanup(func() { _ = mgr.Close(t.Context()) })

	_, err := mgr.runGetTableStats(t.Context(), "public", "")
	require.ErrorContains(t, err, "table must not be empty")
}
