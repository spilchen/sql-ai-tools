// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// End-to-end integration tests for the `crdb-sql mcp` stdio server.
// These spawn the real binary, drive it as an MCP client over JSON-RPC,
// and assert the envelope shape returned by every registered tool. The
// goal is to catch regressions that unit tests cannot — broken tool
// registration, transport serialization bugs, mark3labs/mcp-go upgrade
// incompatibilities — by exercising the same path Claude Code uses.
//
// The binary is built once per test binary into a temp dir; a fresh
// subprocess and MCP client are spawned per top-level test, so failure
// in one test cannot poison another. The single cluster-dependent test
// (explain_sql) calls cockroachtest.Shared, which skips cleanly when
// COCKROACH_BIN / CRDB_TEST_DSN are absent — so this file runs under
// `make test` and additionally exercises explain_sql under
// `make test-integration`.

package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/testutil/cockroachtest"
)

// cmdPkgPath is the import path of the crdb-sql main package that
// buildBinary compiles into the test temp dir. The integration suite
// builds its own binary rather than reusing whatever `make build`
// produced so the tests are self-contained — they do not require a
// prior make invocation and cannot be silently invalidated by a stale
// bin/crdb-sql.
const cmdPkgPath = "github.com/spilchen/sql-ai-tools/cmd/crdb-sql"

// callTimeout bounds every Tier 1/2 tools/call round-trip. These
// complete in milliseconds; five seconds leaves headroom for a loaded
// CI runner without masking a hung subprocess. Tier 3 (explain_sql)
// uses its own larger timeout at the call site because a cold-cluster
// EXPLAIN can exceed callTimeout.
const callTimeout = 5 * time.Second

// initTimeout bounds the MCP initialize handshake. Larger than
// callTimeout because cold subprocess startup plus the initialize
// round-trip can be noticeably slower than steady-state tool calls
// on loaded CI.
const initTimeout = 10 * time.Second

// Shared build state. Populated once by buildBinary on the first call
// (typically from TestMain); subsequent callers get the cached
// (binPath, buildErr) atomically through buildOnce.
//
//   - binPath is the absolute path to the built crdb-sql binary, or
//     "" if the build failed.
//   - buildErr is nil on success or the wrapped go-build error
//     (including captured stderr) on failure.
//   - buildOnce gates the build so concurrent or repeat callers do
//     not re-invoke `go build`.
var (
	binPath   string
	buildErr  error
	buildOnce sync.Once
)

// TestMain builds the crdb-sql binary once into a temp directory and
// then defers to cockroachtest.RunTests so the (optional) shared
// CockroachDB cluster used by the explain_sql test is torn down on
// exit. Build failures abort the test binary with a clear message
// rather than letting every individual test fail with the same error.
//
// The temp directory containing the binary is intentionally not
// removed: cockroachtest.RunTests calls os.Exit, which bypasses
// deferred cleanup. The OS reaps os.TempDir() entries eventually, and
// the binary is small.
func TestMain(m *testing.M) {
	if err := buildBinary(); err != nil {
		fmt.Fprintf(os.Stderr, "integration test setup: %v\n", err)
		os.Exit(1)
	}
	cockroachtest.RunTests(m)
}

// buildBinary compiles the crdb-sql binary into a temp directory and
// records the path in binPath. Idempotent via sync.Once so callers
// outside TestMain pay the cost at most once. On failure the returned
// error embeds the go-build stderr so CI output points directly at
// the compilation problem rather than just "exit status 1".
func buildBinary() error {
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "crdb-sql-mcp-int-")
		if err != nil {
			buildErr = fmt.Errorf("mkdir temp: %w", err)
			return
		}
		out := filepath.Join(dir, "crdb-sql")
		// Capture stderr separately so a failed build surfaces the
		// compiler diagnostics in the wrapped error rather than
		// scattering them across the test runner's output.
		var stderr bytes.Buffer
		cmd := exec.Command("go", "build", "-o", out, cmdPkgPath)
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			buildErr = fmt.Errorf("go build: %w\n%s", err, stderr.String())
			return
		}
		binPath = out
	})
	return buildErr
}

// newMCPClient spawns a fresh `crdb-sql mcp` subprocess, runs the MCP
// initialize handshake, and registers a t.Cleanup to close the client
// (which kills the subprocess) and dump the subprocess's stderr on a
// failed test. Each top-level test gets its own client to keep
// failures isolated.
//
// The subprocess inherits the parent process env (passed as nil to
// NewStdioMCPClient) so COCKROACH_BIN / CRDB_TEST_DSN reach the
// explain_sql Tier 3 path. Replacing nil with []string{} would
// silently break the explain test on systems where those vars are
// the only way to reach a cluster.
func newMCPClient(t *testing.T) *client.Client {
	t.Helper()
	require.NoError(t, buildBinary(), "build crdb-sql binary")

	c, err := client.NewStdioMCPClient(binPath, nil /* env */, "mcp")
	require.NoError(t, err, "spawn crdb-sql mcp")

	// Drain the subprocess stderr into a buffer so a panic, log line,
	// or unexpected diagnostic from the server is visible to the
	// developer when the test fails. Without this, a transport-level
	// failure shows up as a generic "tools/call X failed" with no
	// hint at what the server actually printed — defeating the
	// purpose of an integration suite that exists to catch product
	// regressions in the stdio loop.
	var stderr bytes.Buffer
	stderrDone := make(chan struct{})
	if r, ok := client.GetStderr(c); ok {
		go func() {
			defer close(stderrDone)
			_, _ = io.Copy(&stderr, r)
		}()
	} else {
		close(stderrDone)
	}

	t.Cleanup(func() {
		// Errors on Close are not test failures — the subprocess may
		// already have exited cleanly — but we surface them via t.Log
		// alongside any captured stderr so a developer chasing a
		// failure sees what the server printed before dying.
		if err := c.Close(); err != nil {
			t.Logf("close MCP client: %v", err)
		}
		<-stderrDone
		if t.Failed() && stderr.Len() > 0 {
			t.Logf("crdb-sql mcp stderr:\n%s", stderr.String())
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), initTimeout)
	defer cancel()

	var req mcp.InitializeRequest
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{Name: "crdb-sql-int-test", Version: "0.0.0"}
	_, err = c.Initialize(ctx, req)
	require.NoError(t, err, "MCP initialize handshake")

	return c
}

// callTool issues a tools/call request with the given name and
// arguments under the standard callTimeout. Returns the raw
// CallToolResult so callers can inspect IsError before decoding the
// envelope.
func callTool(t *testing.T, c *client.Client, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()

	var req mcp.CallToolRequest
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := c.CallTool(ctx, req)
	require.NoError(t, err, "tools/call %s", name)
	return res
}

// decodeEnvelope extracts the single TextContent from a successful
// tool result and unmarshals it as an output.Envelope. Fails the test
// if the result is a tool-level error or if the content shape is not
// what every tool in this server emits (one TextContent of JSON).
func decodeEnvelope(t *testing.T, res *mcp.CallToolResult) output.Envelope {
	t.Helper()
	require.False(t, res.IsError, "expected envelope-shaped success result, got tool error: %s", textOf(res))
	require.Len(t, res.Content, 1, "expected exactly one content element")
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok, "expected TextContent, got %T", res.Content[0])

	var env output.Envelope
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &env), "unmarshal envelope: %s", tc.Text)
	return env
}

// textOf returns the concatenated text of a CallToolResult's
// TextContent entries. Used in assertion messages so a failed
// envelope-shape check shows the actual server text.
func textOf(res *mcp.CallToolResult) string {
	var s string
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			s += tc.Text
		}
	}
	return s
}
