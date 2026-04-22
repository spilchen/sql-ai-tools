// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

//go:build integration

// Integration tests for `crdb-sql ping` exercising the full CLI path
// against a real CockroachDB cluster. Build-tagged so `make test`
// stays fast; run via `make test-integration`. The shared cluster is
// provided by the cockroachtest harness (see
// internal/testutil/cockroachtest); it spins up `cockroach demo
// --background` once per test binary or honors CRDB_TEST_DSN.

package cmd

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/conn"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/testutil/cockroachtest"
)

// uuidPattern matches the canonical 8-4-4-4-12 hex-with-dashes UUID
// form. ClusterID is rendered by crdb_internal.cluster_id() in
// canonical form, so a regex match avoids depending on a uuid
// package. Unanchored on purpose: the JSON envelope test matches
// against an info.ClusterID substring inside `data`, and the text
// test matches inside the `cluster_id: <uuid>` line.
var uuidPattern = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// versionPattern matches the leading prefix of a real CockroachDB
// `version()` string. Tighter than a Contains check; tolerates the
// CCL/OSS variation between demo build flavors.
var versionPattern = regexp.MustCompile(`CockroachDB (CCL|OSS) v\d+\.\d+`)

func TestMain(m *testing.M) { cockroachtest.RunTests(m) }

// TestIntegrationPingCmdText covers the default text output: three
// lines listing cluster_id, version, and connection_status.
func TestIntegrationPingCmdText(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	t.Setenv("CRDB_DSN", "")

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"ping", "--dsn", cluster.DSN})

	require.NoError(t, root.Execute())

	out := stdout.String()
	require.Regexp(t, `cluster_id: `+uuidPattern.String(), out)
	require.Regexp(t, `version: `+versionPattern.String(), out)
	require.Contains(t, out, "connection_status: connected")
}

// TestIntegrationPingCmdJSON covers the structured envelope output:
// tier=connected, connection_status=connected, no errors, and Data
// decodes into a populated ClusterInfo.
func TestIntegrationPingCmdJSON(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	t.Setenv("CRDB_DSN", "")

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"ping", "--output", "json", "--dsn", cluster.DSN})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, output.TierConnected, env.Tier)
	require.Equal(t, output.ConnectionConnected, env.ConnectionStatus)
	require.Empty(t, env.Errors)

	var info conn.ClusterInfo
	require.NoError(t, json.Unmarshal(env.Data, &info))
	require.Regexp(t, uuidPattern, info.ClusterID,
		"cluster ID should be a canonical UUID")
	require.Regexp(t, versionPattern, info.Version,
		"version should look like CockroachDB CCL/OSS vN.N…")
}

// TestIntegrationPingCmdConnectionFailureText verifies the text-mode
// error path. RenderError returns the underlying error unchanged in
// text mode (see internal/output/render.go), so cobra/main.go own
// the user-visible "Error: ..." print. The test pins two contracts:
// (1) Execute returns a non-nil non-ErrRendered error carrying the
// "connect to CockroachDB" prefix, and (2) stdout stays empty
// because the success-path text writer was never invoked.
func TestIntegrationPingCmdConnectionFailureText(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	t.Setenv("CRDB_DSN", "")

	badDSN := withPort(t, cluster.DSN, 1)

	root := newRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"ping", "--dsn", badDSN})

	err := root.Execute()
	require.Error(t, err)
	require.NotErrorIs(t, err, output.ErrRendered,
		"text mode must return the raw error, not the JSON-rendered sentinel")
	require.Contains(t, strings.ToLower(err.Error()), "connect to cockroachdb")
	require.Empty(t, stdout.String(),
		"text mode must not print any partial cluster info on the failure path")
}

// TestIntegrationPingCmdConnectionFailureJSON verifies the
// disconnected-envelope path: a bad-port variant of the live DSN
// makes the command surface a structured error rather than crash, and
// connection_status is reported as disconnected.
func TestIntegrationPingCmdConnectionFailureJSON(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	t.Setenv("CRDB_DSN", "")

	badDSN := withPort(t, cluster.DSN, 1)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"ping", "--output", "json", "--dsn", badDSN})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)
	require.NotEmpty(t, env.Errors)
	require.Contains(t, strings.ToLower(env.Errors[0].Message), "connect to cockroachdb")
}

// withPort returns a copy of dsn with its host port replaced by
// newPort. Used to fabricate a closed-port DSN from the live one.
func withPort(t *testing.T, dsn string, newPort int) string {
	t.Helper()
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	host := u.Hostname()
	u.Host = host + ":" + strconv.Itoa(newPort)
	return u.String()
}
