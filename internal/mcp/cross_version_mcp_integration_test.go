// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

//go:build integration

// End-to-end test for issues #129 and #145: a single long-lived
// `crdb-sql mcp` server forwards each tool call to the sibling
// backend whose parser quarter matches the call's target_version,
// reusing a warm pooled child for repeated calls to the same
// target. Builds both the latest crdb-sql (v262) and the
// crdb-sql-v261 sibling into a tempdir, then spawns one MCP server
// and issues parse_sql calls — one for each quarter, plus a repeat
// of the routed (v261) call to exercise the pool's warm-reuse
// path. The envelope's parser_version field on each response proves
// which sibling actually executed the call. Integration-tagged
// because it pays the `go build` cost twice; run via
// `make test-integration`.

package mcp_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/output"
)

// crossVersionParserDivergentStmt is a statement whose grammar
// landed in cockroachdb-parser v0.26.2; v0.26.1 rejects it with a
// SQLSTATE 42601 syntax error. The CLI-side mirror in
// internal/versionroute/cross_version_integration_test.go uses the
// same statement so a future grammar change that erases the
// divergence breaks both tests in lockstep — preventing a silent
// "both quarters now accept it" green-but-meaningless outcome.
const crossVersionParserDivergentStmt = "ALTER TABLE t ENABLE TRIGGER tr"

// TestPerCallMCPTargetVersionRouting is the demo for issues #129
// and #145. One MCP server, multiple tool calls in the same
// session, two different parsers exercised — proven by the
// envelope's parser_version field. The routed-call case runs twice
// back-to-back to exercise the pool's warm-reuse path: the second
// call must hit the same warm sibling and produce identical output.
func TestPerCallMCPTargetVersionRouting(t *testing.T) {
	repoRoot := findMCPRepoRoot(t)
	binDir := t.TempDir()
	latest := filepath.Join(binDir, "crdb-sql")
	sibling := filepath.Join(binDir, "crdb-sql-v261")

	buildCrdbSQLBin(t, repoRoot, latest, "v262", "")
	buildCrdbSQLBin(t, repoRoot, sibling, "v261", "build/go.v261.mod")

	// Prepend the build dir so the latest binary's per-call router
	// finds crdb-sql-v261 via versionroute.FindBackend ($PATH walk).
	pathWithBin := binDir + string(os.PathListSeparator) + os.Getenv("PATH")
	env := append([]string(nil), os.Environ()...)
	env = append(env, "PATH="+pathWithBin)

	c, err := client.NewStdioMCPClient(latest, env, "mcp")
	require.NoError(t, err, "spawn crdb-sql mcp")
	t.Cleanup(func() {
		// A close error here typically signals the sibling crashed
		// or hung mid-session — exactly the regression the
		// integration test exists to catch — so promote it from a
		// log line to a failure.
		if err := c.Close(); err != nil {
			t.Errorf("close MCP client: %v", err)
		}
	})

	initCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var initReq mcp.InitializeRequest
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "crdb-sql-cross-version-test", Version: "0.0.0"}
	_, err = c.Initialize(initCtx, initReq)
	require.NoError(t, err, "MCP initialize handshake")

	tests := []struct {
		name                  string
		targetVersion         string
		expectedParserVersion string
		expectSyntaxError     bool
	}{
		{
			name:                  "latest quarter handled locally",
			targetVersion:         "26.2.0",
			expectedParserVersion: "v0.26.2",
		},
		{
			name:                  "older quarter routed to sibling",
			targetVersion:         "26.1.0",
			expectedParserVersion: "v0.26.1",
			expectSyntaxError:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			callCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var req mcp.CallToolRequest
			req.Params.Name = "parse_sql"
			req.Params.Arguments = map[string]any{
				"sql":            crossVersionParserDivergentStmt,
				"target_version": tc.targetVersion,
			}
			res, err := c.CallTool(callCtx, req)
			require.NoError(t, err, "tools/call parse_sql")
			require.False(t, res.IsError, "expected envelope-shaped success result")

			require.Len(t, res.Content, 1)
			tcText, ok := res.Content[0].(mcp.TextContent)
			require.True(t, ok, "expected TextContent, got %T", res.Content[0])

			var env output.Envelope
			require.NoError(t, json.Unmarshal([]byte(tcText.Text), &env), "decode envelope: %s", tcText.Text)

			require.Equal(t, tc.expectedParserVersion, env.ParserVersion,
				"parser_version on the envelope is the routing proof; mismatch means the call hit the wrong sibling")

			if tc.expectSyntaxError {
				// The v261 parser does not know ENABLE TRIGGER; the
				// envelope must surface the parse error rather than
				// silently producing a successful AST.
				var sawSyntaxErr bool
				for _, e := range env.Errors {
					if e.Code == "42601" {
						sawSyntaxErr = true
						break
					}
				}
				require.True(t, sawSyntaxErr,
					"expected SQLSTATE 42601 in envelope errors for v26.1 parse of v26.2-only syntax: %+v", env.Errors)
			}
		})
	}

	t.Run("warm pool reuses sibling for repeat routed call", func(t *testing.T) {
		// Issue #145: a second routed call to the same target_version
		// must reuse the warm pooled child rather than spawning a
		// fresh sibling. This subtest runs after the first routed
		// call above has already populated the pool with the v261
		// child, so the reuse path is the only way it can succeed.
		// We assert on output equivalence (same parser_version, same
		// SQLSTATE) — not on timing — because timing assertions are
		// flake-prone on CI. A regression that broke pool reuse
		// (e.g. spawn-per-call resurrected) would still produce the
		// right output, but TestPoolWarmReuse pins that case at the
		// unit level.
		callCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var req mcp.CallToolRequest
		req.Params.Name = "parse_sql"
		req.Params.Arguments = map[string]any{
			"sql":            crossVersionParserDivergentStmt,
			"target_version": "26.1.0",
		}
		res, err := c.CallTool(callCtx, req)
		require.NoError(t, err, "warm-reuse routed call must succeed")
		require.False(t, res.IsError, "expected envelope-shaped success result")
		require.Len(t, res.Content, 1)
		tcText, ok := res.Content[0].(mcp.TextContent)
		require.True(t, ok)
		var env output.Envelope
		require.NoError(t, json.Unmarshal([]byte(tcText.Text), &env))
		require.Equal(t, "v0.26.1", env.ParserVersion,
			"warm-reuse call must still hit the v261 sibling")
		var sawSyntaxErr bool
		for _, e := range env.Errors {
			if e.Code == "42601" {
				sawSyntaxErr = true
				break
			}
		}
		require.True(t, sawSyntaxErr, "warm-reuse call must surface the same v261 parse error")
	})

	t.Run("missing sibling produces tool error with discovery hint", func(t *testing.T) {
		// target_version=25.4.0 needs crdb-sql-v254, which we
		// never built. Two things must hold end-to-end:
		//   1. the result is an MCP tool error (IsError=true), not
		//      a JSON-RPC transport error — proves PoolRouter is
		//      wired (NoopRouter would also produce an error here,
		//      but its message would say "routing not enabled"
		//      rather than "not installed");
		//   2. the message names the missing backend and lists
		//      the v261 sibling we did install — proves the
		//      writeMissingBackendError-style discovery hint
		//      survived the proxy → wrapper → MCP transport hop.
		callCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var req mcp.CallToolRequest
		req.Params.Name = "parse_sql"
		req.Params.Arguments = map[string]any{
			"sql":            "SELECT 1",
			"target_version": "25.4.0",
		}
		res, err := c.CallTool(callCtx, req)
		require.NoError(t, err, "missing sibling must surface as a tool error, not a JSON-RPC transport error")
		require.True(t, res.IsError, "missing sibling must be a tool-level error")

		require.Len(t, res.Content, 1)
		tcText, ok := res.Content[0].(mcp.TextContent)
		require.True(t, ok)
		require.Contains(t, tcText.Text, "crdb-sql-v254",
			"missing-backend message must name the requested sibling")
		require.Contains(t, tcText.Text, "not installed",
			"missing-backend message must say the backend is not installed (proves PoolRouter, not NoopRouter)")
		require.Contains(t, tcText.Text, "crdb-sql-v261",
			"missing-backend message must list the discovered v261 sibling under 'Available backends'")
	})
}

// findMCPRepoRoot returns the directory containing the worktree's
// go.mod. Mirrors the CLI-side helper in
// internal/versionroute/cross_version_integration_test.go; duplicated
// rather than shared because cross-package test helpers would
// require an internal/testutil shim that exists for one caller.
func findMCPRepoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").Output()
	require.NoError(t, err)
	gomod := strings.TrimSpace(string(out))
	require.NotEmpty(t, gomod, "go env GOMOD returned empty; not inside a module?")
	return filepath.Dir(gomod)
}

// buildCrdbSQLBin compiles the crdb-sql binary at outputPath,
// stamping the supplied quarter into versionroute.builtQuarterStamp.
// When modfile is non-empty, builds against build/<modfile> (the
// per-quarter sibling's go.mod replace directive); otherwise uses
// the worktree's top-level go.mod. Mirrors the helper in
// internal/versionroute/cross_version_integration_test.go.
func buildCrdbSQLBin(t *testing.T, repoRoot, outputPath, quarter, modfile string) {
	t.Helper()
	args := []string{"build"}
	if modfile != "" {
		args = append(args, "-modfile="+modfile)
	}
	args = append(args,
		"-ldflags=-X github.com/spilchen/sql-ai-tools/internal/versionroute.builtQuarterStamp="+quarter,
		"-o", outputPath, "./cmd/crdb-sql")
	cmd := exec.Command("go", args...)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "go build (quarter=%s, modfile=%s) failed: %s", quarter, modfile, out)
}
