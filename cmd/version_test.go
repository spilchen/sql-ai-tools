// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"bytes"
	"encoding/json"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/output"
)

// TestVersionCmd exercises the `version` subcommand end-to-end through
// a fresh root command.
//
// The binary line is pinned exactly: Version is the package var "dev"
// under `go test` (release stamping happens via -ldflags only).
//
// The parser line is checked for shape only. Under `go test`,
// debug.ReadBuildInfo().Deps does not list cockroachdb-parser even
// though the cmd package's blank import pulls it into the test binary,
// so parserVersion("dev") returns "unknown" here. The full resolution
// path (replace present, version-only, missing dep) is covered by
// TestParserVersionFrom against a synthetic *debug.BuildInfo.
func TestVersionCmd(t *testing.T) {
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"version"})

	require.NoError(t, root.Execute())

	got := buf.String()
	require.Contains(t, got, "crdb-sql: dev\n",
		"binary line should report Version verbatim under go test")

	parserLine := extractLine(got, "cockroachdb-parser: ")
	require.NotEmpty(t, parserLine,
		"version output missing cockroachdb-parser line; got:\n%s", got)
}

// TestVersionCmdJSON exercises --output json end-to-end. The
// parser_version is checked for shape (see TestVersionCmd's note on
// why it resolves to "unknown" under go test); binary_version inside
// data is pinned to "dev" since release stamping happens via -ldflags.
func TestVersionCmdJSON(t *testing.T) {
	root := newRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"version", "--output", "json"})

	require.NoError(t, root.Execute())

	// JSON mode contract: stdout is the single source of truth.
	// Anything on stderr would force agents to merge two streams.
	require.Empty(t, stderr.String(), "JSON mode must not write to stderr")
	require.NotContains(t, stdout.String(), "crdb-sql: ",
		"JSON mode must not also emit text-mode lines")

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.NotEmpty(t, env.ParserVersion, "parser_version must be populated")
	require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)
	require.Equal(t, output.TierUnset, env.Tier, "version has no tier")
	require.Empty(t, env.Errors)

	var data struct {
		BinaryVersion string `json:"binary_version"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Equal(t, "dev", data.BinaryVersion)
}

// TestRootRejectsBadOutput locks in the PersistentPreRunE validation:
// any --output value other than text/json must fail with a message
// naming the valid choices.
func TestRootRejectsBadOutput(t *testing.T) {
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"version", "--output", "xml"})

	err := root.Execute()
	require.Error(t, err)
	require.ErrorContains(t, err, `"text"`)
	require.ErrorContains(t, err, `"json"`)
}

// TestVersionCmdRejectsExtraArgs locks in the cobra.NoArgs contract on
// the version subcommand: any positional arg beyond `version` itself
// must produce a non-nil error from Execute. An accidental switch to
// cobra.ArbitraryArgs would silently regress this and would be caught
// here.
func TestVersionCmdRejectsExtraArgs(t *testing.T) {
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"version", "oops"})

	require.Error(t, root.Execute())
}

// TestParserVersionFrom covers each resolution branch of
// parserVersionFrom by feeding it a synthetic *debug.BuildInfo. This
// is the path that's invisible to TestVersionCmd because `go test`
// does not populate ReadBuildInfo().Deps.
func TestParserVersionFrom(t *testing.T) {
	tests := []struct {
		name           string
		deps           []*debug.Module
		binVersion     string
		expected       string
		expectedErrSub string
	}{
		{
			name: "replace version wins over placeholder",
			deps: []*debug.Module{
				{
					Path:    parserModulePath,
					Version: "v0.0.0-00010101000000-000000000000",
					Replace: &debug.Module{Version: "v0.26.2"},
				},
			},
			binVersion: devVersion,
			expected:   "v0.26.2",
		},
		{
			name: "dep version used when no replace",
			deps: []*debug.Module{
				{Path: parserModulePath, Version: "v0.26.2"},
			},
			binVersion: devVersion,
			expected:   "v0.26.2",
		},
		{
			name:       "missing dep tolerated for dev build",
			deps:       []*debug.Module{{Path: "example.com/other", Version: "v1.0.0"}},
			binVersion: devVersion,
			expected:   "unknown",
		},
		{
			name:           "missing dep is hard error for stamped build",
			deps:           []*debug.Module{{Path: "example.com/other", Version: "v1.0.0"}},
			binVersion:     "v1.2.3",
			expectedErrSub: "missing from build info",
		},
		{
			name: "dep present but version empty falls through to missing",
			deps: []*debug.Module{
				{Path: parserModulePath, Replace: &debug.Module{Version: ""}},
			},
			binVersion: devVersion,
			expected:   "unknown",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			info := &debug.BuildInfo{Deps: tc.deps}
			got, err := parserVersionFrom(info, tc.binVersion)
			if tc.expectedErrSub != "" {
				require.ErrorContains(t, err, tc.expectedErrSub)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expected, got)
		})
	}
}

// extractLine returns the substring following prefix on the line where
// it appears, with the trailing newline stripped. Returns "" if prefix
// is not found.
func extractLine(s, prefix string) string {
	idx := strings.Index(s, prefix)
	if idx < 0 {
		return ""
	}
	rest := s[idx+len(prefix):]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[:nl]
	}
	return rest
}
