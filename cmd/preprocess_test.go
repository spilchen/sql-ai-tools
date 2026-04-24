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

	"github.com/spilchen/sql-ai-tools/internal/output"
)

// messySQL is the verbatim transcript paste from issue #161. The CLI
// surface must accept it on every Tier 1 subcommand and surface the
// input_preprocessed warning, mirroring the MCP test suite.
const messySQL = "root@localhost:26257/movr> SELECT r.city,r.id AS ride_id,\n" +
	"                        -> u.name AS rider_name,v.type AS vehicle_type,\n" +
	"                        ->    r.start_address,r.end_address,\n" +
	"                        -> r.revenue FROM rides r INNER JOIN\n" +
	"                        -> users u ON r.rider_id=u.id AND r.city=u.city INNER JOIN vehicles v\n" +
	"                        ->       ON r.vehicle_id=v.id AND\n" +
	"                        -> r.vehicle_city=v.city WHERE r.city='new york'\n" +
	"                        ->  AND r.revenue > 50.00\n" +
	"                        -> ORDER BY r.revenue DESC;\n"

// runCmdJSON drives a CLI subcommand end to end with messy SQL fed via
// stdin and --output=json. It returns the parsed envelope so callers
// can assert on it. fail=true means we expect ErrRendered (or any
// non-nil error) — used by Tier 3 tests where the cluster contact
// fails. For Tier 1 the call must succeed.
func runCmdJSON(t *testing.T, sql string, args ...string) output.Envelope {
	t.Helper()
	root := newRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader(sql))
	root.SetArgs(append([]string{"--output", "json"}, args...))

	// Some subcommands return ErrRendered on a failed run while still
	// emitting a complete envelope on stdout; we accept either nil or
	// non-nil and assert on the envelope shape instead.
	_ = root.Execute()

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env), "stdout=%q stderr=%q", stdout.String(), stderr.String())
	return env
}

// requireInputPreprocessedWarning extracts the single
// input_preprocessed warning entry from env.Errors and pins its
// shape: WARNING severity, bytes_removed > 0. Mirrors the MCP-side
// helper so the two surfaces stay observable through the same
// assertion path.
func requireInputPreprocessedWarning(t *testing.T, env output.Envelope) {
	t.Helper()
	var found *output.Error
	for i := range env.Errors {
		if env.Errors[i].Code == output.CodeInputPreprocessed {
			require.Nil(t, found, "more than one input_preprocessed warning: %+v", env.Errors)
			found = &env.Errors[i]
		}
	}
	require.NotNil(t, found, "no input_preprocessed warning found in %+v", env.Errors)
	require.Equal(t, output.SeverityWarning, found.Severity)
	require.Greater(t, found.Context["bytes_removed"], float64(0))
}

// TestPreprocessTier1CLISubcommandsEmitWarning verifies every Tier 1
// CLI subcommand accepts the messy.sql fixture and surfaces the
// input_preprocessed warning. The data payload must also be populated
// so the run is observably successful.
func TestPreprocessTier1CLISubcommandsEmitWarning(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "parse", args: []string{"parse"}},
		{name: "validate", args: []string{"validate"}},
		{name: "summarize", args: []string{"summarize"}},
		{name: "risk", args: []string{"risk"}},
		{name: "format", args: []string{"format"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := runCmdJSON(t, messySQL, tc.args...)
			requireInputPreprocessedWarning(t, env)
			require.NotEmpty(t, env.Data, "data payload must be populated for messy SQL after stripping")
			for _, e := range env.Errors {
				require.NotEqual(t, output.SeverityError, e.Severity,
					"unexpected ERROR-severity entry on success path: %+v", e)
			}
		})
	}
}

// TestPreprocessIsNoOpForNonPasteCLIInput pins the hot-path contract:
// SQL with no REPL prompts must not trigger the input_preprocessed
// warning on the CLI surface either.
func TestPreprocessIsNoOpForNonPasteCLIInput(t *testing.T) {
	env := runCmdJSON(t, "SELECT 1;", "parse")
	for _, e := range env.Errors {
		require.NotEqual(t, output.CodeInputPreprocessed, e.Code,
			"input_preprocessed must not fire for prompt-free SQL: %+v", e)
	}
}

// TestPreprocessTier3CLISubcommandsEmitWarning verifies the Tier 3
// CLI subcommands strip the prompt and surface input_preprocessed
// even when the cluster contact fails. An unreachable --dsn forces
// the cluster step to fail fast; the assertion is that the warning
// sits in the envelope alongside the connect error rather than being
// overwritten by it. Mirrors TestPreprocessTier3ToolsEmitWarningOnMessyPaste
// in internal/mcp/tools/preprocess_test.go for the MCP surface.
func TestPreprocessTier3CLISubcommandsEmitWarning(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "explain", args: []string{"explain", "--dsn", "postgres://flaghost:26257/db?connect_timeout=1"}},
		{name: "explain-ddl", args: []string{"explain-ddl", "--mode", "safe_write",
			"--dsn", "postgres://flaghost:26257/db?connect_timeout=1"}},
		{name: "simulate", args: []string{"simulate", "--dsn", "postgres://flaghost:26257/db?connect_timeout=1"}},
		{name: "exec", args: []string{"exec", "--dsn", "postgres://flaghost:26257/db?connect_timeout=1"}},
	}
	// explain-ddl rejects SELECT under any mode, so feed it a DDL paste.
	const ddlMessy = "root@localhost:26257/movr> ALTER TABLE rides\n" +
		"                        -> ADD COLUMN flagged BOOL DEFAULT false;\n"

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := messySQL
			if tc.name == "explain-ddl" {
				input = ddlMessy
			}
			env := runCmdJSON(t, input, tc.args...)
			requireInputPreprocessedWarning(t, env)
		})
	}
}

// TestPreprocessCLIParseErrorTranslatesPosition pins the CLI-side
// position-translation contract. Mirrors the MCP-surface test
// TestPreprocessParseSQLTranslatesPositionToOriginal so a regression
// in renderParseErrorTranslated (cmd/preprocess.go) is caught
// independently of the MCP code path.
func TestPreprocessCLIParseErrorTranslatesPosition(t *testing.T) {
	const prompt = "root@localhost:26257/movr> "
	env := runCmdJSON(t, prompt+"GARB AGE\n", "parse")

	requireInputPreprocessedWarning(t, env)
	var parseErr *output.Error
	for i := range env.Errors {
		if env.Errors[i].Severity == output.SeverityError {
			parseErr = &env.Errors[i]
			break
		}
	}
	require.NotNil(t, parseErr, "expected a parse error in env.Errors: %+v", env.Errors)
	require.NotNil(t, parseErr.Position, "parse error must carry a Position so the agent can locate the typo")

	// In stripped-buffer coordinates the error sits at line 1 column 1
	// (the 'G' of "GARB AGE"). In the original it sits at column
	// len(prompt)+1 — that's what the agent must see.
	require.Equal(t, 1, parseErr.Position.Line)
	require.Equal(t, len(prompt)+1, parseErr.Position.Column,
		"position must be re-derived against the original input, not the stripped buffer")
	require.Equal(t, len(prompt), parseErr.Position.ByteOffset)
}

// TestPreprocessValidateKeepsCapabilityRequired pins that the
// input_preprocessed warning sits ALONGSIDE other warnings, not
// instead of them. validate_sql with messy SQL and no schemas should
// produce both input_preprocessed (stripping fired) and
// capability_required (name resolution skipped). A regression that
// reverted the append-based envelope handling to assignment would
// drop one of the two.
func TestPreprocessValidateKeepsCapabilityRequired(t *testing.T) {
	env := runCmdJSON(t, messySQL, "validate")

	requireInputPreprocessedWarning(t, env)

	var sawCapability bool
	for _, e := range env.Errors {
		if e.Code == "capability_required" {
			sawCapability = true
			break
		}
	}
	require.True(t, sawCapability,
		"capability_required warning must sit alongside input_preprocessed: %+v", env.Errors)
}
