// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

//go:build integration

package conn_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/conn"
	"github.com/spilchen/sql-ai-tools/internal/safety"
	"github.com/spilchen/sql-ai-tools/internal/testutil/cockroachtest"
)

// TestIntegrationManagerExecuteReadOnlySelect covers the read_only
// happy path: a SELECT round-trips through Execute and returns rows,
// columns, and a SELECT command tag. The asserts pin both the data
// and the metadata pieces so a regression that drops either is loud.
func TestIntegrationManagerExecuteReadOnlySelect(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	mgr := conn.NewManager(cluster.DSN)
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := mgr.Execute(ctx, "SELECT 1 AS n, 'hi'::STRING AS s", conn.ExecuteOptions{
		Mode: safety.ModeReadOnly,
	})
	require.NoError(t, err)
	require.Len(t, res.Columns, 2)
	require.Equal(t, "n", res.Columns[0].Name)
	require.Equal(t, "s", res.Columns[1].Name)
	require.Equal(t, 1, res.RowsReturned)
	require.Len(t, res.Rows, 1)
	require.Equal(t, int64(1), res.Rows[0][0])
	require.Equal(t, "hi", res.Rows[0][1])
	require.True(t, strings.HasPrefix(res.CommandTag, "SELECT"),
		"SELECT command tag expected, got %q", res.CommandTag)
}

// TestIntegrationManagerExecuteReadOnlyRejectsWrite pins the
// defense-in-depth contract: even if a caller bypasses safety.Check
// and submits a write under read_only, the cluster's BEGIN READ ONLY
// wrapper rejects with SQLSTATE 25006.
func TestIntegrationManagerExecuteReadOnlyRejectsWrite(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	dbName := uniqueDBName(t, "exec_ro_write")

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

	_, err := mgr.Execute(ctx, "INSERT INTO t VALUES (1)", conn.ExecuteOptions{
		Mode: safety.ModeReadOnly,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "25006",
		"read-only-txn rejection must carry SQLSTATE 25006")
}

// TestIntegrationManagerExecuteSafeWriteInsertReturning pins
// safe_write's happy path: DML succeeds, RETURNING surfaces rows, and
// RowsAffected reflects the cluster's command tag.
func TestIntegrationManagerExecuteSafeWriteInsertReturning(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	dbName := uniqueDBName(t, "exec_sw_insert")

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

	res, err := mgr.Execute(ctx, "INSERT INTO t VALUES (1), (2) RETURNING id",
		conn.ExecuteOptions{Mode: safety.ModeSafeWrite})
	require.NoError(t, err)
	require.Equal(t, 2, res.RowsReturned)
	require.Equal(t, int64(2), res.RowsAffected)
	require.Len(t, res.Columns, 1)
	require.Equal(t, "id", res.Columns[0].Name)
}

// TestIntegrationManagerExecuteSafeWriteRejectsBareUpdate pins the
// runtime guard added by SET LOCAL sql_safe_updates: an UPDATE without
// a WHERE clause is rejected by the cluster even though the AST
// allowlist admits it (safe_write doesn't introspect for WHERE).
func TestIntegrationManagerExecuteSafeWriteRejectsBareUpdate(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	dbName := uniqueDBName(t, "exec_sw_bare_update")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mustExec(t, ctx, cluster.DSN, "CREATE DATABASE "+dbName)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		mustExec(t, ctx, cluster.DSN, "DROP DATABASE IF EXISTS "+dbName+" CASCADE")
	})
	mustExec(t, ctx, cluster.DSN,
		"CREATE TABLE "+dbName+".public.t (id INT PRIMARY KEY, v INT)")
	mustExec(t, ctx, cluster.DSN,
		"INSERT INTO "+dbName+".public.t VALUES (1, 1)")

	mgr := conn.NewManager(dsnWithDatabase(t, cluster.DSN, dbName))
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	_, err := mgr.Execute(ctx, "UPDATE t SET v = 2", conn.ExecuteOptions{
		Mode: safety.ModeSafeWrite,
	})
	require.Error(t, err,
		"sql_safe_updates must reject UPDATE without WHERE")
	require.Contains(t, strings.ToLower(err.Error()), "without where",
		"error should name the missing WHERE clause")
}

// TestIntegrationManagerExecuteFullAccessDDL pins the full_access
// contract: schema changes succeed without escalation hints because
// the AST allowlist admits everything that parses.
func TestIntegrationManagerExecuteFullAccessDDL(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	dbName := uniqueDBName(t, "exec_fa_ddl")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mustExec(t, ctx, cluster.DSN, "CREATE DATABASE "+dbName)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		mustExec(t, ctx, cluster.DSN, "DROP DATABASE IF EXISTS "+dbName+" CASCADE")
	})

	mgr := conn.NewManager(dsnWithDatabase(t, cluster.DSN, dbName))
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	_, err := mgr.Execute(ctx, "CREATE TABLE x (id INT PRIMARY KEY)",
		conn.ExecuteOptions{Mode: safety.ModeFullAccess})
	require.NoError(t, err, "full_access must admit DDL")
}

// TestIntegrationManagerExecuteTruncatesAtMaxRows pins the MaxRows
// guardrail: scanning stops when the cap is hit and the result reports
// Truncated=true. The statement still runs to completion on the
// cluster, but the agent gets a bounded payload. Also pins that the
// CommandTag and RowsAffected are correctly populated even on the
// truncation path — this is load-bearing because pgx populates the
// command tag inside Close, and a regression that read the tag before
// Close would silently produce CommandTag="" and RowsAffected=0.
func TestIntegrationManagerExecuteTruncatesAtMaxRows(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	mgr := conn.NewManager(cluster.DSN)
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := mgr.Execute(ctx,
		"SELECT generate_series(1, 5) AS n",
		conn.ExecuteOptions{Mode: safety.ModeReadOnly, MaxRows: 2})
	require.NoError(t, err)
	require.Equal(t, 2, res.RowsReturned, "scan must stop at MaxRows")
	require.True(t, res.Truncated, "Truncated flag must be set")
	require.NotEmpty(t, res.CommandTag,
		"CommandTag must be populated on truncation (pgx populates it in Close)")
	require.True(t, strings.HasPrefix(res.CommandTag, "SELECT"),
		"CommandTag must report the cluster's authoritative tag, got %q", res.CommandTag)
}

// TestIntegrationManagerExecuteRecoversAfterError pins the
// connection-recovery contract documented on Execute: an error after
// a successful connect closes the cached connection and nils it so
// the next call re-dials transparently. Mirrors
// TestIntegrationManagerExplainDDLRecoversAfterError so the two
// surfaces stay diff-friendly.
func TestIntegrationManagerExecuteRecoversAfterError(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	mgr := conn.NewManager(cluster.DSN)
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Force a cluster-side error after the lazy connect has succeeded.
	_, err := mgr.Execute(ctx, "SELECT * FROM table_that_does_not_exist",
		conn.ExecuteOptions{Mode: safety.ModeReadOnly})
	require.Error(t, err, "SELECT against missing table must surface an error")

	// Second call must succeed against a real query — proving the
	// Manager re-dialed and is not stuck on the closed connection.
	res, err := mgr.Execute(ctx, "SELECT 1",
		conn.ExecuteOptions{Mode: safety.ModeReadOnly})
	require.NoError(t, err, "Execute after a prior failure must reconnect")
	require.Equal(t, 1, res.RowsReturned)
}

// TestIntegrationManagerExecuteRejectsUnknownMode pins the mode
// validation added to runExecute: an unrecognised mode token must
// fail loudly with no transaction opened, rather than silently
// falling through to a read-write txn shape (which would be silent
// privilege escalation if a future caller bypassed safety.ParseMode).
func TestIntegrationManagerExecuteRejectsUnknownMode(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	mgr := conn.NewManager(cluster.DSN)
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := mgr.Execute(ctx, "SELECT 1",
		conn.ExecuteOptions{Mode: safety.Mode("yolo")})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown safety mode",
		"unknown mode must surface a structured error, not silently use a default")
}

// TestIntegrationManagerExecuteEnforcesStatementTimeout mirrors the
// matching test for Explain: an aggressive 1ms timeout forces a slow
// statement to fail with SQLSTATE 57014.
func TestIntegrationManagerExecuteEnforcesStatementTimeout(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	mgr := conn.NewManager(cluster.DSN, conn.WithStatementTimeout(1*time.Millisecond))
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := mgr.Execute(ctx, "SELECT pg_sleep(5)",
		conn.ExecuteOptions{Mode: safety.ModeReadOnly})
	require.Error(t, err)
	require.Contains(t, err.Error(), "57014",
		"timeout error must carry SQLSTATE 57014 (query_canceled)")
}
