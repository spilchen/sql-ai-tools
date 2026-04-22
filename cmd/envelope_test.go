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

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/output"
)

func TestNewEnvelope(t *testing.T) {
	t.Run("sets tier and connection status", func(t *testing.T) {
		state := &rootState{outputFormat: output.FormatJSON}
		cmd := &cobra.Command{}
		cmd.SetOut(&bytes.Buffer{})

		r, env, err := newEnvelope(state, output.TierZeroConfig, cmd)
		require.NoError(t, err)
		require.Equal(t, output.FormatJSON, r.Format)
		require.Equal(t, output.TierZeroConfig, env.Tier)
		require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)
		require.NotEmpty(t, env.ParserVersion)
	})

	t.Run("TierUnset omits tier from JSON", func(t *testing.T) {
		state := &rootState{outputFormat: output.FormatJSON}
		cmd := &cobra.Command{}
		cmd.SetOut(&bytes.Buffer{})

		_, env, err := newEnvelope(state, output.TierUnset, cmd)
		require.NoError(t, err)
		require.Equal(t, output.TierUnset, env.Tier)

		data, err := json.Marshal(env)
		require.NoError(t, err)
		require.NotContains(t, string(data), `"tier"`)
	})

	t.Run("no target version omits field and emits no warning", func(t *testing.T) {
		state := &rootState{outputFormat: output.FormatJSON}
		cmd := &cobra.Command{}
		cmd.SetOut(&bytes.Buffer{})

		_, env, err := newEnvelope(state, output.TierZeroConfig, cmd)
		require.NoError(t, err)
		require.Empty(t, env.TargetVersion)
		require.Empty(t, env.Errors)

		data, err := json.Marshal(env)
		require.NoError(t, err)
		require.NotContains(t, string(data), `"target_version"`)
	})

	t.Run("target version stamps field on envelope", func(t *testing.T) {
		// VersionMismatchWarning's logic is exercised end-to-end in
		// internal/output/version_test.go. Asserting warning emission
		// here would require stubbing the parser version: under
		// `go test` parserVersion(Version) returns "unknown" because
		// the test binary's debug.ReadBuildInfo does not list the
		// cockroachdb-parser dep the same way the production binary
		// does. So this case verifies only the field-stamping wiring;
		// the warning predicate is covered as a unit test.
		state := &rootState{
			outputFormat:  output.FormatJSON,
			targetVersion: "25.4.0",
		}
		cmd := &cobra.Command{}
		cmd.SetOut(&bytes.Buffer{})

		_, env, err := newEnvelope(state, output.TierZeroConfig, cmd)
		require.NoError(t, err)
		require.Equal(t, "25.4.0", env.TargetVersion)
	})
}

// TestRootRejectsMalformedTargetVersion confirms PersistentPreRunE
// surfaces the validation error before any subcommand RunE runs, so
// the user sees a clear message instead of a partially-constructed
// envelope. The error is also wrapped with the flag name so users
// know which flag failed when several are passed.
func TestRootRejectsMalformedTargetVersion(t *testing.T) {
	root := newRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader("SELECT 1"))
	root.SetArgs([]string{"parse", "--target-version", "not-a-version"})

	err := root.Execute()
	require.ErrorContains(t, err, "--target-version")
	require.ErrorContains(t, err, "target version")
	require.ErrorContains(t, err, "not-a-version",
		"the offending value must appear in the message so the user can spot the typo")
}

// TestRootCanonicalizesTargetVersion is the positive counterpart to
// TestRootRejectsMalformedTargetVersion. It runs `parse` end-to-end
// with --target-version v25.4.0 and asserts the envelope reports
// the canonical (no-"v") form. A future regression that drops the
// canonicalization assignment in PersistentPreRunE would surface as
// "v25.4.0" in the JSON output.
func TestRootCanonicalizesTargetVersion(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("SELECT 1"))
	root.SetArgs([]string{"parse", "--target-version", "v25.4.0", "--output", "json"})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, "25.4.0", env.TargetVersion,
		"PersistentPreRunE must store the canonical (no-'v') form")
}
