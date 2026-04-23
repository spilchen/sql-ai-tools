// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

//go:build integration

// Integration tests for `crdb-sql exec` exercising the full CLI path
// against a real CockroachDB cluster. Build-tagged so `make test`
// stays fast; run via `make test-integration`. Mirrors the structure
// of ping_integration_test.go.

package cmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/conn"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/testutil/cockroachtest"
)

// TestIntegrationExecCmdLimitInjectionReachesEnvelope pins the
// end-to-end LIMIT-injection contract of the CLI: an unbounded
// SELECT under read_only with --max-rows must come back with
// limit_injected populated in the envelope. The unit tests cover
// MaybeInjectLimit and the renderer in isolation, but only an
// integration call exercises the cmd/exec.go plumbing that copies
// the cap onto ExecuteResult.LimitInjected after the rewrite ran.
func TestIntegrationExecCmdLimitInjectionReachesEnvelope(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	t.Setenv("CRDB_DSN", "")

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"exec",
		"--output", "json",
		"--dsn", cluster.DSN,
		"--max-rows", "3",
		"-e", "SELECT generate_series(1, 10) AS n",
	})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, output.ConnectionConnected, env.ConnectionStatus)
	require.Empty(t, env.Errors)

	var res conn.ExecuteResult
	require.NoError(t, json.Unmarshal(env.Data, &res))
	require.NotNil(t, res.LimitInjected,
		"unbounded SELECT under read_only must surface limit_injected")
	require.Equal(t, 3, *res.LimitInjected)
	require.Equal(t, 3, res.RowsReturned,
		"injected LIMIT 3 must cap the result to 3 rows")
}

// TestIntegrationExecCmdNoLimitInjectionWhenBounded pins the
// negative case: a SELECT that already has a LIMIT must not get
// rewritten, and limit_injected must be absent from the envelope.
func TestIntegrationExecCmdNoLimitInjectionWhenBounded(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	t.Setenv("CRDB_DSN", "")

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"exec",
		"--output", "json",
		"--dsn", cluster.DSN,
		"--max-rows", "1000",
		"-e", "SELECT generate_series(1, 5) AS n LIMIT 5",
	})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	var res conn.ExecuteResult
	require.NoError(t, json.Unmarshal(env.Data, &res))
	require.Nil(t, res.LimitInjected,
		"caller-supplied LIMIT must not trigger injection")
	require.Equal(t, 5, res.RowsReturned)
}

// TestIntegrationExecCmdEnforcesTimeoutFlag pins that the --timeout
// CLI flag actually reaches conn.Manager. The conn-layer test pins
// the SQLSTATE 57014 contract on the Manager itself; this CLI test
// verifies the flag-binding plumbing (a regression that shadowed the
// variable or used the wrong default would silently revert to 30s
// and the manager-level test would still pass).
func TestIntegrationExecCmdEnforcesTimeoutFlag(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	t.Setenv("CRDB_DSN", "")

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"exec",
		"--output", "json",
		"--dsn", cluster.DSN,
		"--timeout", "1ms",
		"-e", "SELECT pg_sleep(5)",
	})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.Contains(t, env.Errors[0].Message, "57014",
		"--timeout=1ms must produce a SQLSTATE 57014 error from the cluster")
	require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus,
		"a cluster-side timeout error leaves the manager's recovery contract in place — connection_status reverts")
}
