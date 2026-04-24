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

// runExec executes `crdb-sql exec` with the supplied args and stdin,
// returning the captured stdout buffer and the Execute error. Mirrors
// runExplain so the two surfaces stay diff-friendly.
func runExec(t *testing.T, stdin string, args ...string) (*bytes.Buffer, error) {
	t.Helper()
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"exec"}, args...))
	return &stdout, root.Execute()
}

// TestExecCmdNoDSN verifies that exec without a DSN fails before any
// cluster contact, naming both the flag and the env var.
func TestExecCmdNoDSN(t *testing.T) {
	t.Setenv("CRDB_DSN", "")

	_, err := runExec(t, "", "-e", "SELECT 1")
	require.Error(t, err)
	require.ErrorContains(t, err, "no connection string")
}

// TestExecCmdSafetyRejection verifies that the read_only allowlist
// short-circuits each mutating statement before any cluster contact.
// The DSN points at an unreachable host on purpose: a regression that
// stops short-circuiting would surface a connect error instead of the
// safety_violation we expect.
//
// Some rows pass --mode read_only explicitly to exercise the
// flag-binding path (a regression that ignored --mode and always
// used the default would silently pass these without that
// distinction).
func TestExecCmdSafetyRejection(t *testing.T) {
	tests := []struct {
		name         string
		sql          string
		passModeFlag bool
		expectedTag  string
	}{
		{name: "delete (default mode)", sql: "DELETE FROM t WHERE id = 1", expectedTag: "DELETE"},
		{name: "insert (default mode)", sql: "INSERT INTO t VALUES (1)", expectedTag: "INSERT"},
		{name: "update (explicit read_only)", sql: "UPDATE t SET x = 1 WHERE id = 1", passModeFlag: true, expectedTag: "UPDATE"},
		{name: "drop table (explicit read_only)", sql: "DROP TABLE users", passModeFlag: true, expectedTag: "DROP TABLE"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"--output", "json",
				"--dsn", "postgres://nope:1/db?connect_timeout=1"}
			if tc.passModeFlag {
				args = append(args, "--mode", "read_only")
			}
			args = append(args, "-e", tc.sql)
			stdout, err := runExec(t, "", args...)
			require.ErrorIs(t, err, output.ErrRendered)

			var env output.Envelope
			require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
			require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus,
				"safety rejection must short-circuit before any cluster contact")
			require.Len(t, env.Errors, 1)
			require.Equal(t, output.CodeSafetyViolation, env.Errors[0].Code)
			require.Equal(t, tc.expectedTag, env.Errors[0].Context["tag"])
			require.Equal(t, "read_only", env.Errors[0].Context["mode"])
			require.Equal(t, "execute", env.Errors[0].Context["operation"])
		})
	}
}

// TestExecCmdSafetyRejectionSuggestsEscalation verifies the
// asymmetric escalation hints end-to-end. The decision is driven by
// Violation.Kind, not Reason wording, so each row pins the
// minimum-mode contract for one (Mode, Kind) cell.
//
// Critically, GRANT under read_only must escalate straight to
// full_access — not safe_write, which itself rejects DCL and would
// loop the agent through a second rejection.
func TestExecCmdSafetyRejectionSuggestsEscalation(t *testing.T) {
	tests := []struct {
		name                string
		mode                string
		sql                 string
		expectedReplacement string
	}{
		{
			name:                "write under read_only suggests safe_write",
			mode:                "read_only",
			sql:                 "INSERT INTO t VALUES (1)",
			expectedReplacement: "safe_write",
		},
		{
			name:                "ddl under read_only suggests full_access",
			mode:                "read_only",
			sql:                 "CREATE TABLE x (id INT PRIMARY KEY)",
			expectedReplacement: "full_access",
		},
		{
			name:                "dcl under read_only suggests full_access (skips safe_write loop)",
			mode:                "read_only",
			sql:                 "GRANT SELECT ON t TO bob",
			expectedReplacement: "full_access",
		},
		{
			name:                "ddl under safe_write suggests full_access",
			mode:                "safe_write",
			sql:                 "CREATE TABLE x (id INT PRIMARY KEY)",
			expectedReplacement: "full_access",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CRDB_DSN", "")
			stdout, err := runExec(t, "", "--output", "json",
				"--dsn", "postgres://nope:1/db?connect_timeout=1",
				"--mode", tc.mode,
				"-e", tc.sql)
			require.ErrorIs(t, err, output.ErrRendered)

			var env output.Envelope
			require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
			require.Len(t, env.Errors, 1)
			require.Len(t, env.Errors[0].Suggestions, 1)
			require.Equal(t, tc.expectedReplacement, env.Errors[0].Suggestions[0].Replacement)
		})
	}
}

// TestExecCmdInvalidMode verifies that --mode rejects unknown values
// before any input or cluster contact.
func TestExecCmdInvalidMode(t *testing.T) {
	t.Setenv("CRDB_DSN", "postgres://envhost:26257/defaultdb")

	stdout, err := runExec(t, "", "--output", "json",
		"--mode", "yolo",
		"-e", "SELECT 1")
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.Contains(t, env.Errors[0].Message, "invalid safety mode")
}

// plpgsqlExecVersionWarningSQL is the PL/pgSQL fixture shared by the
// exec version-warning tests. plpgsql_function_body is registered as
// introduced in v24.1 (see internal/version/registry.go), so any
// --target-version older than that triggers a feature_not_yet_introduced
// warning when version.Inspect runs over the parsed AST.
const plpgsqlExecVersionWarningSQL = `CREATE FUNCTION f() RETURNS INT LANGUAGE PLpgSQL AS $$ BEGIN RETURN 1; END $$`

// TestExecCmdVersionWarning_PLpgSQL pins that --target-version below
// the feature's Introduced version emits a feature_not_yet_introduced
// WARNING into env.Errors AND that the warning survives a downstream
// safety_violation (the append-not-overwrite invariant the parse
// handler tests pin on its own surface). CREATE FUNCTION is DDL, so
// the read_only allowlist rejects it before any cluster contact —
// connection_status stays disconnected and the test does not need a
// reachable cluster.
func TestExecCmdVersionWarning_PLpgSQL(t *testing.T) {
	stdout, err := runExec(t, "",
		"--target-version", "23.2",
		"--dsn", "postgres://nope:1/db",
		"--output", "json",
		"-e", plpgsqlExecVersionWarningSQL)
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, "23.2", env.TargetVersion)

	// Filter rather than index: the envelope may also carry a
	// target_version_mismatch warning (parser is on v0.26 in this
	// build) and the safety_violation appended after the warning.
	var featWarn *output.Error
	for i := range env.Errors {
		if env.Errors[i].Code == output.CodeFeatureNotYetIntroduced {
			featWarn = &env.Errors[i]
			break
		}
	}
	require.NotNilf(t, featWarn, "expected a feature_not_yet_introduced warning in %+v", env.Errors)
	require.Equal(t, output.SeverityWarning, featWarn.Severity)
	require.Equal(t, "plpgsql_function_body", featWarn.Context["feature_tag"])
	require.Equal(t, "24.1", featWarn.Context["introduced"])
	require.Equal(t, "23.2", featWarn.Context["target"])

	var safetyErr *output.Error
	for i := range env.Errors {
		if env.Errors[i].Code == output.CodeSafetyViolation {
			safetyErr = &env.Errors[i]
			break
		}
	}
	require.NotNilf(t, safetyErr,
		"version warning must coexist with the safety violation in %+v", env.Errors)
}

// TestExecCmdVersionWarning_NoneAtNewerTarget pins the negative case:
// when target is at or after the feature's Introduced version, no
// feature warning is emitted.
func TestExecCmdVersionWarning_NoneAtNewerTarget(t *testing.T) {
	stdout, err := runExec(t, "",
		"--target-version", "24.1",
		"--dsn", "postgres://nope:1/db",
		"--output", "json",
		"-e", plpgsqlExecVersionWarningSQL)
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	for _, e := range env.Errors {
		require.NotEqualf(t, output.CodeFeatureNotYetIntroduced, e.Code,
			"target at Introduced must not warn, got %+v", e)
	}
}

// TestExecCmdVersionWarning_NoFlagSkips covers the documented
// short-circuit: omitting --target-version skips the inspector
// entirely. Without this, a regression that promoted "" to "warn
// anyway" would only surface on the parse / validate / summarize
// surfaces.
func TestExecCmdVersionWarning_NoFlagSkips(t *testing.T) {
	stdout, err := runExec(t, "",
		"--dsn", "postgres://nope:1/db",
		"--output", "json",
		"-e", plpgsqlExecVersionWarningSQL)
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Empty(t, env.TargetVersion)
	for _, e := range env.Errors {
		require.NotEqualf(t, output.CodeFeatureNotYetIntroduced, e.Code,
			"no --target-version must skip inspector, got %+v", e)
	}
}

// TestRenderExecText covers the text-mode rendering branches:
// tabular output for SELECT-shape results, command-tag-only output for
// DML without RETURNING, the truncated trailer, and the LIMIT-injected
// trailer. Pure rendering test; no cluster needed.
func TestRenderExecText(t *testing.T) {
	limit := 1000
	tests := []struct {
		name             string
		input            conn.ExecuteResult
		expectedContains []string
	}{
		{
			name: "tabular select",
			input: conn.ExecuteResult{
				Columns:      []conn.ColumnMeta{{Name: "id"}, {Name: "name"}},
				Rows:         [][]any{{int64(1), "alice"}, {int64(2), "bob"}},
				RowsReturned: 2,
				CommandTag:   "SELECT 2",
			},
			expectedContains: []string{"id", "name", "alice", "bob", "(2 rows)"},
		},
		{
			name: "tabular truncated",
			input: conn.ExecuteResult{
				Columns:      []conn.ColumnMeta{{Name: "n"}},
				Rows:         [][]any{{int64(1)}, {int64(2)}},
				RowsReturned: 2,
				Truncated:    true,
			},
			expectedContains: []string{"(2 rows, truncated)"},
		},
		{
			name: "command tag only",
			input: conn.ExecuteResult{
				CommandTag:   "INSERT 0 5",
				RowsAffected: 5,
			},
			expectedContains: []string{"INSERT 0 5"},
		},
		{
			name: "limit injection annotated",
			input: conn.ExecuteResult{
				Columns:       []conn.ColumnMeta{{Name: "n"}},
				Rows:          [][]any{{int64(1)}},
				RowsReturned:  1,
				LimitInjected: &limit,
			},
			expectedContains: []string{"(1 rows)", "LIMIT 1000 injected"},
		},
		{
			name: "null rendered as uppercase token",
			input: conn.ExecuteResult{
				Columns:      []conn.ColumnMeta{{Name: "n"}},
				Rows:         [][]any{{nil}},
				RowsReturned: 1,
			},
			expectedContains: []string{"NULL"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, renderExecText(&buf, tc.input))
			out := buf.String()
			for _, want := range tc.expectedContains {
				require.Contains(t, out, want)
			}
		})
	}
}
