// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/risk"
	"github.com/spilchen/sql-ai-tools/internal/summarize"
)

// TestSummarizeCmdText exercises the text output path end-to-end with
// the issue's demo SQL.
func TestSummarizeCmdText(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("DELETE FROM orders WHERE status='x'"))
	root.SetArgs([]string{"summarize"})

	require.NoError(t, root.Execute())

	got := stdout.String()
	require.Contains(t, got, "operation:")
	require.Contains(t, got, "DELETE")
	require.Contains(t, got, "orders")
	require.Contains(t, got, "status = 'x'")
	// New rows added for issue #100 must appear in text output.
	require.Contains(t, got, "referenced_columns:")
	require.Contains(t, got, "status")
	require.Contains(t, got, "select_star:")
	require.Contains(t, got, "false")
}

// TestSummarizeCmdSelectStarJSON verifies that select_star is
// serialized as a true boolean (not omitted, not "true" string) and
// that referenced_columns is emitted as an empty JSON array rather
// than null when a bare * leaves no other refs.
func TestSummarizeCmdSelectStarJSON(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"summarize", "-e", "SELECT * FROM t", "--output", "json"})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	// Wire-format anchors (the renderer pretty-prints, so match the
	// indented form): select_star must serialize as a true bool,
	// not be omitted; referenced_columns must be an empty array,
	// not null.
	require.Contains(t, string(env.Data), `"select_star": true`)
	require.Contains(t, string(env.Data), `"referenced_columns": []`)

	var summaries []summarize.Summary
	require.NoError(t, json.Unmarshal(env.Data, &summaries))
	require.Len(t, summaries, 1)
	require.True(t, summaries[0].SelectStar)
	require.Empty(t, summaries[0].ReferencedColumns)
}

// TestSummarizeCmdJSONIssueDemo verifies that the issue's demo
// command produces the documented JSON shape exactly.
func TestSummarizeCmdJSONIssueDemo(t *testing.T) {
	root := newRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"summarize", "-e", "DELETE FROM orders WHERE status='x'", "--output", "json"})

	require.NoError(t, root.Execute())
	require.Empty(t, stderr.String(), "JSON mode must not write to stderr")

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, output.TierZeroConfig, env.Tier)
	require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)
	require.NotEmpty(t, env.ParserVersion)
	require.Empty(t, env.Errors)

	var summaries []summarize.Summary
	require.NoError(t, json.Unmarshal(env.Data, &summaries))
	require.Len(t, summaries, 1)

	s := summaries[0]
	require.Equal(t, summarize.OpDelete, s.Operation)
	require.Equal(t, []string{"orders"}, s.Tables)
	require.Equal(t, []string{"status = 'x'"}, s.Predicates)
	require.Empty(t, s.Joins)
	require.Empty(t, s.AffectedColumns)
	require.Equal(t, []string{"status"}, s.ReferencedColumns)
	require.False(t, s.SelectStar)
	require.Equal(t, risk.SeverityInfo, s.RiskLevel)
}

// TestSummarizeCmdRiskDelegation verifies that summarize's risk_level
// matches what the risk subcommand would produce for the same SQL.
func TestSummarizeCmdRiskDelegation(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"summarize", "-e", "DELETE FROM orders", "--output", "json"})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))

	var summaries []summarize.Summary
	require.NoError(t, json.Unmarshal(env.Data, &summaries))
	require.Len(t, summaries, 1)
	require.Equal(t, risk.SeverityCritical, summaries[0].RiskLevel)
}

// TestSummarizeCmdFileArg verifies reading SQL from a file argument.
func TestSummarizeCmdFileArg(t *testing.T) {
	dir := t.TempDir()
	sqlFile := filepath.Join(dir, "input.sql")
	require.NoError(t, os.WriteFile(sqlFile, []byte("UPDATE t SET a=1 WHERE id=2"), 0644))

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"summarize", sqlFile, "--output", "json"})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))

	var summaries []summarize.Summary
	require.NoError(t, json.Unmarshal(env.Data, &summaries))
	require.Len(t, summaries, 1)
	require.Equal(t, summarize.OpUpdate, summaries[0].Operation)
	require.Equal(t, []string{"a"}, summaries[0].AffectedColumns)
}

// TestSummarizeCmdMultiStatement verifies that each statement gets
// its own summary in the JSON payload.
func TestSummarizeCmdMultiStatement(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"summarize", "-e", "SELECT 1 FROM a JOIN b ON a.id=b.id; DROP TABLE foo", "--output", "json"})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))

	var summaries []summarize.Summary
	require.NoError(t, json.Unmarshal(env.Data, &summaries))
	require.Len(t, summaries, 2)
	require.Equal(t, summarize.OpSelect, summaries[0].Operation)
	require.Len(t, summaries[0].Joins, 1)
	require.Equal(t, summarize.OpOther, summaries[1].Operation)
	require.Equal(t, "DROP TABLE", summaries[1].Tag)
	require.Equal(t, risk.SeverityCritical, summaries[1].RiskLevel)
}

// TestSummarizeCmdParseErrorJSON verifies that invalid SQL in JSON
// mode produces an envelope with errors and nil data.
func TestSummarizeCmdParseErrorJSON(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("SELECTT 1"))
	root.SetArgs([]string{"summarize", "--output", "json"})

	err := root.Execute()
	require.Error(t, err)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.NotEmpty(t, env.Errors)
	require.Nil(t, env.Data)
}

// TestSummarizeCmdEmptyInput verifies that empty stdin produces an
// error (matching the rest of the SQL-consuming subcommands).
func TestSummarizeCmdEmptyInput(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader(""))
	root.SetArgs([]string{"summarize"})

	require.Error(t, root.Execute())
}

// plpgsqlSummarizeWarningSQL is the PL/pgSQL fixture used by the
// summarize-side version-warning tests. It mirrors the fixture used by
// TestParseCmdVersionWarning_PLpgSQL so a regression in either path is
// caught locally.
const plpgsqlSummarizeWarningSQL = `CREATE FUNCTION f() RETURNS INT LANGUAGE PLpgSQL AS $$ BEGIN RETURN 1; END $$`

// TestSummarizeCmdVersionWarning_PLpgSQL is the summarize mirror of
// TestParseCmdVersionWarning_PLpgSQL: --target-version=23.2 with PL/pgSQL
// emits a feature_not_yet_introduced warning while the data payload (the
// per-statement summaries) still populates.
func TestSummarizeCmdVersionWarning_PLpgSQL(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"summarize",
		"--target-version", "23.2",
		"-e", plpgsqlSummarizeWarningSQL,
		"--output", "json",
	})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, "23.2", env.TargetVersion)

	var got *output.Error
	for i := range env.Errors {
		if env.Errors[i].Code == output.CodeFeatureNotYetIntroduced {
			got = &env.Errors[i]
			break
		}
	}
	require.NotNilf(t, got, "expected a feature_not_yet_introduced warning in %+v", env.Errors)
	require.Equal(t, output.SeverityWarning, got.Severity)
	require.Equal(t, "plpgsql_function_body", got.Context["feature_tag"])
	require.Equal(t, "24.1", got.Context["introduced"])
	require.Equal(t, "23.2", got.Context["target"])
	require.NotEmpty(t, env.Data, "summarize must still succeed and emit a data payload")
}

// TestSummarizeCmdVersionWarning_NoneAtNewerTarget pins the negative
// case: target at or after Introduced emits no feature warning.
func TestSummarizeCmdVersionWarning_NoneAtNewerTarget(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"summarize",
		"--target-version", "24.1",
		"-e", plpgsqlSummarizeWarningSQL,
		"--output", "json",
	})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	for _, e := range env.Errors {
		require.NotEqualf(t, output.CodeFeatureNotYetIntroduced, e.Code,
			"target at Introduced must not warn, got %+v", e)
	}
}

// TestSummarizeCmdVersionWarning_NoFlagSkips covers the documented
// short-circuit: no --target-version means version.Inspect is skipped.
func TestSummarizeCmdVersionWarning_NoFlagSkips(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"summarize",
		"-e", plpgsqlSummarizeWarningSQL,
		"--output", "json",
	})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Empty(t, env.TargetVersion)
	for _, e := range env.Errors {
		require.NotEqualf(t, output.CodeFeatureNotYetIntroduced, e.Code,
			"no --target-version must skip feature warnings, got %+v", e)
	}
}
