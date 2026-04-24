// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package proxy

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/versionroute"
)

// fakeQuarter is a Year.Quarter combination that is extremely
// unlikely to be installed on any developer machine. SpawnRouter's
// missing-backend tests target it so the test does not depend on the
// host having (or not having) any specific sibling binary.
var fakeQuarter = versionroute.Quarter{Year: 99, Q: 4}

// requireToolError asserts that res is a tool-level error and
// returns the concatenated text of every TextContent block so the
// caller can run substring assertions.
func requireToolError(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	require.NotNil(t, res, "expected a result, got nil")
	require.True(t, res.IsError, "expected tool-level error result")
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			text += tc.Text
		}
	}
	require.NotEmpty(t, text, "tool error result must carry a message")
	return text
}

// TestNoopRouterReturnsToolError pins that the default Router does
// not silently fall through. A wiring regression that left NoopRouter
// in place where SpawnRouter was intended must surface as a clear
// "routing not enabled" message rather than a confusing "answered
// with the wrong parser" envelope.
func TestNoopRouterReturnsToolError(t *testing.T) {
	res, err := NoopRouter{}.Dispatch(context.Background(), fakeQuarter, mcp.CallToolRequest{})
	require.NoError(t, err)
	text := requireToolError(t, res)
	require.Contains(t, text, "not enabled")
	require.Contains(t, text, fakeQuarter.BackendName())
}

// TestSpawnRouterMissingBackendReportsDiscoveryHint pins the user-
// facing missing-backend message. The wording deliberately mirrors
// the CLI's writeMissingBackendError so an operator hitting either
// surface sees the same diagnostic shape: which backend was needed,
// what the running binary is, what alternatives exist, and the
// install hint.
func TestSpawnRouterMissingBackendReportsDiscoveryHint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH semantics + 0o755 perm bits differ on Windows; covered by integration test")
	}
	// Empty PATH so the only place FindBackend looks is the test
	// binary's directory, which never contains a crdb-sql-v994.
	t.Setenv("PATH", t.TempDir())

	r := NewSpawnRouter()
	res, err := r.Dispatch(context.Background(), fakeQuarter, mcp.CallToolRequest{})
	require.NoError(t, err, "missing-backend must be a tool error, not a transport error")
	text := requireToolError(t, res)
	require.Contains(t, text, fakeQuarter.BackendName(),
		"message must name the requested backend")
	require.Contains(t, text, "not installed",
		"message must explicitly state the backend is missing")
	require.Contains(t, text, "Install the "+fakeQuarter.Tag()+" backend",
		"message must give the install hint with the backend tag")
}

// TestSpawnRouterMissingBackendListsDiscoveredAlternatives covers
// the missing-backend message's "Available backends" branch by
// dropping a fake sibling into a tempdir on PATH. Without this
// test the discovery-list formatting (column widths, IsSelf
// rendering, BackendName for each entry) is uncovered, and a
// regression that breaks operator-facing diagnostics — e.g.
// rendering paths without their backend names — would slip past
// TestSpawnRouterMissingBackendReportsDiscoveryHint, which
// exercises only the no-backends branch.
func TestSpawnRouterMissingBackendListsDiscoveredAlternatives(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH semantics + 0o755 perm bits differ on Windows; covered by integration test")
	}
	dir := t.TempDir()
	// Drop a fake sibling for v26.1 so Discover finds it. Empty
	// file with the executable bit set is enough — isExecutable
	// keys on the bit, not the contents.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "crdb-sql-v261"), []byte{}, 0o755))
	t.Setenv("PATH", dir)

	r := NewSpawnRouter()
	res, err := r.Dispatch(context.Background(), fakeQuarter, mcp.CallToolRequest{})
	require.NoError(t, err)
	text := requireToolError(t, res)
	require.Contains(t, text, "Available backends:",
		"discovered backends must be listed under the Available backends header")
	require.Contains(t, text, "crdb-sql-v261",
		"the v261 sibling we planted must appear in the discovered list")
	require.Contains(t, text, fakeQuarter.BackendName(),
		"the missing backend's name must still appear so the operator knows what's needed")
}
