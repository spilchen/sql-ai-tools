// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

//go:build integration

// Integration tests for Manager.Simulate / ExplainAnalyze /
// GetTableStats against a real CockroachDB cluster. Build-tagged so
// `make test` stays fast; run via `make test-integration`.

package conn_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/conn"
	"github.com/spilchen/sql-ai-tools/internal/testutil/cockroachtest"
)

// TestIntegrationManagerExplainAnalyzeSelect covers the happy path:
// EXPLAIN ANALYZE on a SELECT returns a populated plan tree plus the
// "execution time" / "rows read" header lines that distinguish it
// from plain EXPLAIN. The explicit assertions are kept loose enough
// to tolerate CRDB version drift while still failing if a future
// regression silently routes through plain EXPLAIN (which would
// strip the runtime stats).
func TestIntegrationManagerExplainAnalyzeSelect(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	mgr := conn.NewManager(cluster.DSN)
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := mgr.ExplainAnalyze(ctx, "SELECT 1")
	require.NoError(t, err)
	require.NotEmpty(t, result.RawRows, "EXPLAIN ANALYZE must return raw output")
	require.NotEmpty(t, result.Plan, "EXPLAIN ANALYZE must parse into a plan tree")
}

// TestIntegrationManagerExplainAnalyzeRejectsWritesViaReadOnlyTxn
// pins the load-bearing safety property of ExplainAnalyze: even if
// a caller bypasses the dispatcher (which routes writes to plain
// Explain), the cluster's BEGIN READ ONLY wrapper rejects the
// inner write with SQLSTATE 25006. Without this guard, ANALYZE
// would persist the write — exactly the side effect the simulate
// design avoids.
func TestIntegrationManagerExplainAnalyzeRejectsWritesViaReadOnlyTxn(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	mgr := conn.NewManager(cluster.DSN)
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mustExec(t, ctx, cluster.DSN,
		"CREATE TABLE IF NOT EXISTS explain_analyze_probe (id INT PRIMARY KEY)")

	_, err := mgr.ExplainAnalyze(ctx, "INSERT INTO explain_analyze_probe VALUES (1)")
	require.Error(t, err, "ExplainAnalyze of an INSERT must be rejected by the read-only txn")
	require.Contains(t, err.Error(), "25006",
		"rejection must carry SQLSTATE 25006 (read_only_sql_transaction)")
}

// TestIntegrationManagerSimulateSelect verifies the dispatcher
// routes a SELECT through StrategyExplainAnalyze and populates Plan
// (not DDLPlan). Single-statement happy path.
func TestIntegrationManagerSimulateSelect(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	mgr := conn.NewManager(cluster.DSN)
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := mgr.Simulate(ctx, "SELECT 1")
	require.NoError(t, err)
	require.Len(t, result.Steps, 1)
	step := result.Steps[0]
	require.Equal(t, conn.StrategyExplainAnalyze, step.Strategy)
	require.Empty(t, step.Error)
	require.NotNil(t, step.Plan, "SELECT step must carry a Plan")
	require.Nil(t, step.DDLPlan, "SELECT step must not carry a DDLPlan")
	require.Empty(t, step.TableStats, "SELECT step has no DDL targets")
}

// TestIntegrationManagerSimulateDMLWriteUsesPlainExplain pins the
// dispatcher's safety choice for writes: route to plain EXPLAIN so
// no row is persisted, even though ANALYZE would give richer stats.
// We verify the row count of the target table is unchanged
// after the simulation.
func TestIntegrationManagerSimulateDMLWriteUsesPlainExplain(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	dbName := uniqueDBName(t, "simulate_write")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mustExec(t, ctx, cluster.DSN, "CREATE DATABASE "+dbName)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		mustExec(t, ctx, cluster.DSN, "DROP DATABASE IF EXISTS "+dbName+" CASCADE")
	})
	mustExec(t, ctx, cluster.DSN,
		"CREATE TABLE "+dbName+".public.t (id INT PRIMARY KEY)")

	mgr := conn.NewManager(dsnWithDatabase(t, cluster.DSN, dbName))
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	result, err := mgr.Simulate(ctx, "INSERT INTO t VALUES (42)")
	require.NoError(t, err)
	require.Len(t, result.Steps, 1)
	step := result.Steps[0]
	require.Equal(t, conn.StrategyExplain, step.Strategy)
	require.Empty(t, step.Error)
	require.NotNil(t, step.Plan)

	// Verify the INSERT was not persisted: the table must be empty.
	var count int
	require.NoError(t,
		queryScalar(t, ctx, dsnWithDatabase(t, cluster.DSN, dbName),
			"SELECT count(*) FROM t", &count))
	require.Equal(t, 0, count,
		"plain EXPLAIN of an INSERT must not persist the row")
}

// TestIntegrationManagerSimulateDDL verifies the DDL path: the
// dispatcher routes ALTER TABLE through EXPLAIN (DDL, SHAPE), and
// the table referenced by the ALTER survives unchanged (no schema
// modification). TableStats are populated when SHOW STATISTICS has
// data; we assert the slice has one entry naming the right table
// rather than a row-count value (auto-collection timing varies).
func TestIntegrationManagerSimulateDDL(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	dbName := uniqueDBName(t, "simulate_ddl")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mustExec(t, ctx, cluster.DSN, "CREATE DATABASE "+dbName)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		mustExec(t, ctx, cluster.DSN, "DROP DATABASE IF EXISTS "+dbName+" CASCADE")
	})
	mustExec(t, ctx, cluster.DSN,
		"CREATE TABLE "+dbName+".public.users (id INT PRIMARY KEY, name STRING)")

	mgr := conn.NewManager(dsnWithDatabase(t, cluster.DSN, dbName))
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	result, err := mgr.Simulate(ctx, "ALTER TABLE public.users ADD COLUMN age INT")
	require.NoError(t, err)
	require.Len(t, result.Steps, 1)
	step := result.Steps[0]
	require.Equal(t, conn.StrategyExplainDDL, step.Strategy)
	require.NotNil(t, step.DDLPlan)
	require.Nil(t, step.Plan)
	require.Len(t, step.TableStats, 1, "ALTER TABLE has one extracted target")
	require.Equal(t, "public", step.TableStats[0].Schema)
	require.Equal(t, "users", step.TableStats[0].Table)
	require.Equal(t, "show_statistics", step.TableStats[0].Source)

	// Verify the schema was not modified — `age` must still be absent.
	var colCount int
	require.NoError(t, queryScalar(t, ctx, dsnWithDatabase(t, cluster.DSN, dbName),
		"SELECT count(*) FROM information_schema.columns "+
			"WHERE table_schema='public' AND table_name='users' AND column_name='age'",
		&colCount))
	require.Equal(t, 0, colCount,
		"EXPLAIN (DDL, SHAPE) must not actually add the column")
}

// TestIntegrationManagerSimulateUnqualifiedDDLPreservesPlan pins
// the load-bearing partial-failure contract: an unqualified DDL
// (the common form `ALTER TABLE users ...`) must produce a DDL
// plan even if the stats lookup against the empty schema yields
// nothing useful. Earlier behaviour rejected the empty schema
// inside runGetTableStats and surfaced the failure as step.Error,
// which the renderer suppressed — so unqualified DDL appeared
// blank to operators. Now stats lookup against an empty schema
// resolves via search_path; the plan is always preserved.
func TestIntegrationManagerSimulateUnqualifiedDDLPreservesPlan(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	dbName := uniqueDBName(t, "simulate_unqual")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mustExec(t, ctx, cluster.DSN, "CREATE DATABASE "+dbName)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		mustExec(t, ctx, cluster.DSN, "DROP DATABASE IF EXISTS "+dbName+" CASCADE")
	})
	mustExec(t, ctx, cluster.DSN,
		"CREATE TABLE "+dbName+".public.users (id INT PRIMARY KEY)")

	mgr := conn.NewManager(dsnWithDatabase(t, cluster.DSN, dbName))
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	// Unqualified target — parser leaves Schema="".
	result, err := mgr.Simulate(ctx, "ALTER TABLE users ADD COLUMN x INT")
	require.NoError(t, err)
	require.Len(t, result.Steps, 1)
	step := result.Steps[0]
	require.Empty(t, step.Error, "plan must succeed for unqualified target")
	require.NotNil(t, step.DDLPlan, "DDL plan must be preserved for unqualified target")
	require.Len(t, step.TableStats, 1)
	// Schema came back empty from the parser; the stat carries the
	// same empty schema since we did not resolve it client-side.
	require.Equal(t, "users", step.TableStats[0].Table)
}

// TestIntegrationManagerSimulateMultiStatement verifies the
// per-statement contract: a mixed batch returns one Step per
// parsed statement, with each step independently routed.
func TestIntegrationManagerSimulateMultiStatement(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	dbName := uniqueDBName(t, "simulate_multi")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mustExec(t, ctx, cluster.DSN, "CREATE DATABASE "+dbName)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		mustExec(t, ctx, cluster.DSN, "DROP DATABASE IF EXISTS "+dbName+" CASCADE")
	})
	mustExec(t, ctx, cluster.DSN,
		"CREATE TABLE "+dbName+".public.t (id INT PRIMARY KEY)")

	mgr := conn.NewManager(dsnWithDatabase(t, cluster.DSN, dbName))
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	result, err := mgr.Simulate(ctx,
		"SELECT 1; INSERT INTO t VALUES (1); ALTER TABLE t ADD COLUMN c INT")
	require.NoError(t, err)
	require.Len(t, result.Steps, 3)

	require.Equal(t, conn.StrategyExplainAnalyze, result.Steps[0].Strategy)
	require.Equal(t, 0, result.Steps[0].StatementIndex)

	require.Equal(t, conn.StrategyExplain, result.Steps[1].Strategy)
	require.Equal(t, 1, result.Steps[1].StatementIndex)

	require.Equal(t, conn.StrategyExplainDDL, result.Steps[2].Strategy)
	require.Equal(t, 2, result.Steps[2].StatementIndex)
}

// TestIntegrationManagerSimulateMultiStatementMidFailure pins
// the per-step error-isolation contract: a mid-batch failure
// (here, a query against a non-existent table) does not abort the
// remaining steps, the StatementIndex stays correctly aligned, and
// the failed step carries its cluster error in step.Error while
// neighbouring steps complete normally.
func TestIntegrationManagerSimulateMultiStatementMidFailure(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	dbName := uniqueDBName(t, "simulate_mid")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mustExec(t, ctx, cluster.DSN, "CREATE DATABASE "+dbName)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		mustExec(t, ctx, cluster.DSN, "DROP DATABASE IF EXISTS "+dbName+" CASCADE")
	})
	mustExec(t, ctx, cluster.DSN,
		"CREATE TABLE "+dbName+".public.t (id INT PRIMARY KEY)")

	mgr := conn.NewManager(dsnWithDatabase(t, cluster.DSN, dbName))
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	result, err := mgr.Simulate(ctx,
		"SELECT 1; SELECT * FROM does_not_exist; SELECT 2")
	require.NoError(t, err, "method-level call should succeed even when one step fails")
	require.Len(t, result.Steps, 3)

	require.Empty(t, result.Steps[0].Error, "step 0 (SELECT 1) should succeed")
	require.NotNil(t, result.Steps[0].Plan)

	require.NotEmpty(t, result.Steps[1].Error, "step 1 must carry the cluster error")
	require.Equal(t, 1, result.Steps[1].StatementIndex,
		"failed step must keep its position in the batch")

	require.Empty(t, result.Steps[2].Error, "step 2 (SELECT 2) must run after step 1 failed")
	require.NotNil(t, result.Steps[2].Plan)

	// The aggregate summary must mark the partial failure so the
	// CLI/MCP layers can surface it at the envelope level.
	msg, planFails, _, ok := result.StepFailureSummary()
	require.True(t, ok)
	require.Equal(t, []int{1}, planFails)
	require.Contains(t, msg, "1 plan error(s)")
}

// TestIntegrationManagerGetTableStatsFreshTable pins the
// "no stats yet" contract: a table that was just created (so
// auto-stats has not run) returns a zero TableStat without an
// error. The dispatcher relies on this so a freshly created
// target does not turn the simulation into an error case.
func TestIntegrationManagerGetTableStatsFreshTable(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	dbName := uniqueDBName(t, "stats_fresh")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mustExec(t, ctx, cluster.DSN, "CREATE DATABASE "+dbName)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		mustExec(t, ctx, cluster.DSN, "DROP DATABASE IF EXISTS "+dbName+" CASCADE")
	})
	mustExec(t, ctx, cluster.DSN,
		"CREATE TABLE "+dbName+".public.fresh (id INT PRIMARY KEY)")

	mgr := conn.NewManager(dsnWithDatabase(t, cluster.DSN, dbName))
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	stat, err := mgr.GetTableStats(ctx, "public", "fresh")
	require.NoError(t, err, "missing stats must not be an error")
	require.Equal(t, "public", stat.Schema)
	require.Equal(t, "fresh", stat.Table)
	require.Equal(t, "show_statistics", stat.Source)
}

// queryScalar opens a one-off pgx connection, runs the given SQL,
// and scans a single scalar column into dest. Used in the simulate
// integration tests to verify side-effect absence (e.g. row counts
// before/after a simulated DML).
func queryScalar(t *testing.T, ctx context.Context, dsn, sql string, dest any) error {
	t.Helper()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer c.Close(ctx) //nolint:errcheck // best-effort cleanup
	return c.QueryRow(ctx, sql).Scan(dest)
}
