// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRootTLSFlagsMergeIntoDSN drives the PersistentPreRunE merge by
// invoking `ping` with an --ssl* flag plus a URI --dsn. The merge runs
// before ping's RunE; we drive it through ping (rather than a fake
// subcommand) so the test exercises the same wiring the user does.
//
// "Closed port" is the canary: if the merge silently dropped the
// flag, ping would fail with a transport error against the URI's
// real port. With the merge, it tries TLS against port 1 and fails
// at the connect step — that's enough to confirm the merge happened
// without needing a real cluster.
func TestRootTLSFlagsMergeIntoDSN(t *testing.T) {
	t.Setenv("CRDB_DSN", "")

	root := newRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{
		"ping",
		"--dsn", "postgres://root@127.0.0.1:1/defaultdb",
		"--sslmode", "verify-full",
		"--sslrootcert", "/nonexistent/ca.crt",
	})

	err := root.Execute()
	// The connect attempt must fail (no cluster on port 1), but the
	// failure must come from the ping path, not from PreRunE — the
	// merge succeeded if we reach a connect-time error.
	require.Error(t, err)
	require.Contains(t, err.Error(), "connect to CockroachDB",
		"merged DSN must reach the ping connect step; got %v", err)
}

// TestRootTLSFlagConflictFailsLoud pins the fail-loud contract: when
// both --dsn and a --ssl* flag supply the same key, PreRunE returns
// the conflict error rather than silently overriding either source.
func TestRootTLSFlagConflictFailsLoud(t *testing.T) {
	t.Setenv("CRDB_DSN", "")

	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"ping",
		"--dsn", "postgres://root@h:26257/db?sslmode=require",
		"--sslmode", "verify-full",
	})

	err := root.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "sslmode")
	require.Contains(t, err.Error(), "already present")
}

// TestRootTLSFlagRejectsKeywordDSN verifies the form policy is wired
// through PreRunE: a TLS flag combined with a keyword/value --dsn
// errors before any subcommand runs, so the user gets a clear "URI
// required" message instead of a downstream pgx parse error.
func TestRootTLSFlagRejectsKeywordDSN(t *testing.T) {
	t.Setenv("CRDB_DSN", "")

	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"ping",
		"--dsn", "host=h port=26257 user=root",
		"--sslmode", "verify-full",
	})

	err := root.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "URI DSN")
}

// TestRootTLSFlagsMergeWithEnvDSN pins that the merge sees the DSN
// resolved from CRDB_DSN, not just from --dsn. The PreRunE order is
// "--dsn beats env, then merge"; a regression that re-ordered the
// merge before the env-var fallback would silently bypass the form
// policy when only env was set.
func TestRootTLSFlagsMergeWithEnvDSN(t *testing.T) {
	t.Setenv("CRDB_DSN", "postgres://root@127.0.0.1:1/defaultdb")

	root := newRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{
		"ping",
		"--sslmode", "verify-full",
	})

	err := root.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "connect to CockroachDB",
		"merge must apply to the env-resolved DSN; got %v", err)
}

// TestRootTLSFlagsRejectMissingDSN pins the most likely first-time
// user error: a --ssl* flag with no DSN (neither --dsn nor CRDB_DSN).
// The merge must fail loudly with a clear "require a DSN" message
// rather than fall through to a downstream "no connection string"
// error from the subcommand.
func TestRootTLSFlagsRejectMissingDSN(t *testing.T) {
	t.Setenv("CRDB_DSN", "")

	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"ping",
		"--sslmode", "verify-full",
	})

	err := root.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "require a DSN")
}

// TestRootTLSFlagsAbsentNoOp verifies the no-op path: when no --ssl*
// flag is set, PreRunE leaves --dsn untouched. We assert this by
// supplying a closed-port URI DSN with no merge inputs and confirming
// the failure still comes from the ping connect step (rather than a
// MergeTLSParams error).
func TestRootTLSFlagsAbsentNoOp(t *testing.T) {
	t.Setenv("CRDB_DSN", "")

	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"ping",
		"--dsn", "postgres://root@127.0.0.1:1/defaultdb",
	})

	err := root.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "connect to CockroachDB")
}
