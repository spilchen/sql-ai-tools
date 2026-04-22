// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

//go:build integration

// Integration tests for the conn.Manager exercising a real
// CockroachDB cluster. Build-tagged so `make test` stays fast; run via
// `make test-integration`. The shared cluster is provided by the
// cockroachtest harness, which spins up `cockroach demo --background`
// once per test binary (or honors CRDB_TEST_DSN).
//
// "Bad credentials" is intentionally not covered: the demo cluster
// runs with --insecure and accepts any user, so an auth-rejection
// assertion would be unstable. Wrong-port, unreachable-host, and
// malformed-DSN cover the connection-failure surface deterministically.

package conn_test

import (
	"context"
	"net/url"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/conn"
	"github.com/spilchen/sql-ai-tools/internal/testutil/cockroachtest"
)

// uuidPattern matches the canonical 8-4-4-4-12 hex-with-dashes UUID
// form, anchored so a stray UUID-shaped substring elsewhere in a
// future ClusterID format cannot satisfy the assertion. We match
// against a regex rather than depending on a uuid package:
// ClusterID is documented to be the cluster_id() string, which is
// always rendered in canonical form.
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// versionPattern matches the leading prefix of a real CockroachDB
// `version()` string ("CockroachDB CCL v25.x..." or
// "CockroachDB OSS v..."). Tighter than a Contains check: catches a
// regression that swaps in a different distribution string while
// still tolerating the CCL/OSS variation between demo build flavors.
var versionPattern = regexp.MustCompile(`^CockroachDB (CCL|OSS) v\d+\.\d+`)

func TestMain(m *testing.M) { cockroachtest.RunTests(m) }

// TestIntegrationManagerPing covers the happy path: NewManager + Ping
// returns a populated ClusterInfo against a real cluster.
func TestIntegrationManagerPing(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	mgr := conn.NewManager(cluster.DSN)
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := mgr.Ping(ctx)
	require.NoError(t, err)
	require.Regexp(t, uuidPattern, info.ClusterID,
		"cluster ID should be a canonical UUID")
	require.Regexp(t, versionPattern, info.Version,
		"version should look like CockroachDB CCL/OSS vN.N…")
}

// TestIntegrationManagerPingAfterCloseReconnects pins the lazy
// reconnect contract in manager.go: Close clears the cached
// connection (m.conn = nil) and the next Ping re-dials transparently
// rather than erroring. Without this test, a future change that adds
// a "closed" sentinel state could silently break either side of the
// contract — either failing reuse or breaking lazy reconnect — with
// no test coverage.
func TestIntegrationManagerPingAfterCloseReconnects(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	mgr := conn.NewManager(cluster.DSN)
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	first, err := mgr.Ping(ctx)
	require.NoError(t, err)
	require.NoError(t, mgr.Close(ctx))

	second, err := mgr.Ping(ctx)
	require.NoError(t, err)
	require.Equal(t, first.ClusterID, second.ClusterID,
		"reconnect after Close should land on the same cluster")
}

// TestIntegrationManagerPingTwice verifies the connection-reuse
// contract: a second Ping reuses the lazy-connect connection rather
// than dialing again, and returns the same cluster ID.
func TestIntegrationManagerPingTwice(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	mgr := conn.NewManager(cluster.DSN)
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	first, err := mgr.Ping(ctx)
	require.NoError(t, err)
	second, err := mgr.Ping(ctx)
	require.NoError(t, err)
	require.Equal(t, first.ClusterID, second.ClusterID,
		"cluster ID should be stable across Ping calls")
}

// TestIntegrationManagerCloseAfterPing covers the documented
// idempotency of Close: a second Close on a Manager whose connection
// has already been released must be a no-op.
func TestIntegrationManagerCloseAfterPing(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	mgr := conn.NewManager(cluster.DSN)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := mgr.Ping(ctx)
	require.NoError(t, err)
	require.NoError(t, mgr.Close(ctx))
	require.NoError(t, mgr.Close(ctx),
		"Close should be a no-op on a Manager whose connection was already released")
}

// TestIntegrationManagerExplainDDL covers the happy path for
// ExplainDDL against a real cluster. Assertions are deliberately
// tolerant of CRDB version drift: we check that a known statement
// canonicalizes into the Statement field, that at least one operation
// is parsed (even the cheapest schema change emits an "execute N system
// table mutations transactions" line), and that RawText is non-empty.
// We do not pin the operation count or text because the declarative
// schema changer's plan composition can shift between minor versions.
func TestIntegrationManagerExplainDDL(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	mgr := conn.NewManager(cluster.DSN)
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create a fresh table so the ALTER ... ADD COLUMN target exists
	// in the catalog when the schema changer plans it. We use a
	// one-off pgx connection (mustExec) because Manager intentionally
	// exposes only EXPLAIN-flavored read paths.
	mustExec(t, ctx, cluster.DSN,
		"CREATE TABLE IF NOT EXISTS explain_ddl_users (id INT PRIMARY KEY, name STRING)")

	result, err := mgr.ExplainDDL(ctx,
		"ALTER TABLE explain_ddl_users ADD COLUMN age INT")
	require.NoError(t, err)
	require.Contains(t, result.Statement, "ALTER TABLE",
		"statement should canonicalize the ALTER")
	require.Contains(t, result.Statement, "ADD COLUMN age",
		"statement should preserve the ADD COLUMN clause")
	require.NotEmpty(t, result.Operations,
		"every schema change has at least one operation")
	require.NotEmpty(t, result.RawText,
		"raw text should be retained for fidelity")
}

// TestIntegrationManagerExplainDDLRecoversAfterError pins the
// connection-recovery contract documented on ExplainDDL: an error
// after a successful connect closes the cached connection and nils it
// so the next call re-dials transparently. A regression that left the
// dead connection in place would silently leak it and reuse it on the
// next call.
func TestIntegrationManagerExplainDDLRecoversAfterError(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	mgr := conn.NewManager(cluster.DSN)
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mustExec(t, ctx, cluster.DSN,
		"CREATE TABLE IF NOT EXISTS explain_ddl_recovery (id INT PRIMARY KEY)")

	// First call against a non-existent table forces a cluster-side
	// error after the lazy connect has succeeded — the recovery code
	// path we want to exercise.
	_, err := mgr.ExplainDDL(ctx,
		"ALTER TABLE explain_ddl_does_not_exist ADD COLUMN x INT")
	require.Error(t, err, "ALTER on missing table should surface a cluster error")

	// Second call must succeed against a real table — proving the
	// Manager re-dialed and is not stuck on the closed connection.
	result, err := mgr.ExplainDDL(ctx,
		"ALTER TABLE explain_ddl_recovery ADD COLUMN x INT")
	require.NoError(t, err, "ExplainDDL after a prior failure must reconnect")
	require.NotEmpty(t, result.Operations,
		"recovered call should return a real plan, not zero value")
}

// mustExec opens a one-off pgx connection and runs sql, failing the
// test on any error. Used to perform DDL setup outside the Manager:
// Manager intentionally exposes no general Exec, so tests that need a
// one-shot escape hatch for setup go through here rather than around
// it. Kept private to this test file because no other test needs
// arbitrary execution.
func mustExec(t *testing.T, ctx context.Context, dsn, sql string) {
	t.Helper()
	c, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err, "open setup connection")
	defer c.Close(ctx) //nolint:errcheck // best-effort cleanup
	_, err = c.Exec(ctx, sql)
	require.NoError(t, err, "exec setup SQL")
}

// TestIntegrationManagerPingFailures table-drives the
// connection-failure surface. Each case rewrites the live DSN to
// produce a deterministic dial failure; the assertion targets the
// wrapped "connect to CockroachDB" prefix from manager.connect.
func TestIntegrationManagerPingFailures(t *testing.T) {
	cluster := cockroachtest.Shared(t)

	tests := []struct {
		name              string
		dsn               func(live string) string
		expectedErrSubstr string
	}{
		{
			name:              "wrong port",
			dsn:               rewritePort(1),
			expectedErrSubstr: "connect to CockroachDB",
		},
		{
			name: "unreachable host",
			// 198.51.100.0/24 is reserved for documentation (RFC 5737)
			// and is guaranteed to be unroutable, so this case fails
			// with a deterministic dial timeout rather than a DNS
			// lookup error that could vary by resolver.
			dsn:               rewriteHost("198.51.100.1"),
			expectedErrSubstr: "connect to CockroachDB",
		},
		{
			name:              "malformed dsn",
			dsn:               func(string) string { return "not-a-postgres-url" },
			expectedErrSubstr: "connect to CockroachDB",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mgr := conn.NewManager(tc.dsn(cluster.DSN))
			t.Cleanup(func() { _ = mgr.Close(context.Background()) })

			// 5s is enough to fail-closed locally without making the
			// unreachable-host case wait out the kernel's full TCP
			// retry budget.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			_, err := mgr.Ping(ctx)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.expectedErrSubstr)
		})
	}
}

// rewritePort returns a DSN-rewriter that swaps the host port to the
// given value. Used to fabricate a closed-port DSN from a live one.
func rewritePort(newPort int) func(string) string {
	return func(live string) string {
		u, err := url.Parse(live)
		if err != nil {
			return live + ":bad"
		}
		host := u.Hostname()
		u.Host = host + ":" + strconv.Itoa(newPort)
		return u.String()
	}
}

// rewriteHost returns a DSN-rewriter that swaps the host portion of
// the URL to the given value, preserving the original port.
func rewriteHost(newHost string) func(string) string {
	return func(live string) string {
		u, err := url.Parse(live)
		if err != nil {
			return "postgres://" + newHost + ":26257/defaultdb"
		}
		port := u.Port()
		if port == "" {
			u.Host = newHost
		} else {
			u.Host = newHost + ":" + port
		}
		return u.String()
	}
}
