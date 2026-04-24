// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

//go:build integration

// TLS-mode integration tests for `crdb-sql ping`. These tests cannot
// reuse the shared cluster fixture (it is insecure), so they spin up
// their own short-lived secure demo cluster via cockroachtest.WithSecure.
//
// Two sub-tests:
//
//   - the URI-only path: hand the secure DSN (with sslmode + cert
//     paths embedded) straight to ping and confirm it connects.
//   - the flag-merge path: strip the CA path out of the URI, pass it
//     back through --sslrootcert, and confirm ping still connects.
//     This is the canary for cmd/root.go's MergeTLSParams wiring; if
//     a regression dropped the flag layer, the second sub-test would
//     fail with a TLS handshake error while the first still passed.
//
// Note on auth flavor: `cockroach demo` uses password auth, so its
// DSN does not carry sslcert/sslkey — the merge canary uses
// sslrootcert (which the demo does publish) instead. The merge logic
// is field-agnostic, so this still pins all four flag wires.

package cmd

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/testutil/cockroachtest"
)

func TestIntegrationPingTLS(t *testing.T) {
	cluster, err := cockroachtest.Start(context.Background(), cockroachtest.WithSecure())
	if errors.Is(err, cockroachtest.ErrBinaryNotFound) {
		// Mirror the harness's Shared() policy: a missing binary is
		// Fatal by default; skip only when CRDB_INTEGRATION_OPTIONAL
		// is set to a truthy value (the CI opt-out). The harness
		// helper is unexported, so the policy is reimplemented here.
		raw := os.Getenv("CRDB_INTEGRATION_OPTIONAL")
		optIn, perr := strconv.ParseBool(raw)
		if raw == "" || perr != nil || !optIn {
			t.Fatalf("cockroachtest: %v; install cockroach on $PATH, set COCKROACH_BIN, or export CRDB_INTEGRATION_OPTIONAL=1 to skip", err)
		}
		t.Skipf("cockroachtest: %v; CRDB_INTEGRATION_OPTIONAL=%q set, skipping", err, raw)
	}
	if err != nil {
		t.Fatalf("start secure cluster: %v", err)
	}
	t.Cleanup(func() {
		if err := cluster.Stop(); err != nil {
			t.Logf("teardown: %v", err)
		}
	})
	t.Setenv("CRDB_DSN", "")

	t.Run("dsn carries TLS params", func(t *testing.T) {
		root := newRootCmd()
		var stdout bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&bytes.Buffer{})
		root.SetArgs([]string{"ping", "--dsn", cluster.DSN})

		require.NoError(t, root.Execute(),
			"ping against secure demo cluster must connect via the demo-issued TLS DSN")
		require.Contains(t, stdout.String(), "connection_status: connected")
	})

	t.Run("flags merge into dsn", func(t *testing.T) {
		strippedDSN, sslrootcert := stripParam(t, cluster.DSN, "sslrootcert")
		require.NotEmpty(t, sslrootcert, "demo cluster must publish sslrootcert in its DSN")

		root := newRootCmd()
		var stdout bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&bytes.Buffer{})
		root.SetArgs([]string{
			"ping",
			"--dsn", strippedDSN,
			"--sslrootcert", sslrootcert,
		})

		require.NoError(t, root.Execute(),
			"ping must reconstruct a working TLS DSN from --sslrootcert; got DSN=%s", strippedDSN)
		require.Contains(t, stdout.String(), "connection_status: connected")
	})
}

// stripParam removes a single query parameter from dsn and returns
// both the remaining DSN and the stripped value. Used to fabricate
// the "user moved a TLS knob from URI to flag" scenario without
// hardcoding cert paths the demo harness picks at random.
func stripParam(t *testing.T, dsn, key string) (string, string) {
	t.Helper()
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	q := u.Query()
	val := q.Get(key)
	q.Del(key)
	u.RawQuery = q.Encode()
	return u.String(), val
}
