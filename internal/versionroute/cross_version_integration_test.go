// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

//go:build integration

// End-to-end test that the per-quarter sibling backends actually
// ship a different parser, not just a different binary name. Builds
// both the latest crdb-sql and the crdb-sql-v261 sibling into a
// tempdir, then runs `parse` against a statement whose grammar
// landed in v0.26.2. The latest must accept it; routing through
// --target-version 26.1.0 must hit the v0.26.1 parser and reject
// it with a syntax error. Integration-tagged because it pays the
// `go build` cost twice; run via `make test-integration`.

package versionroute_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// crossVersionDivergentStmt is a statement whose grammar exists in
// the LATEST_QUARTER parser but NOT in the v26.1 parser. ALTER TABLE
// ... ENABLE TRIGGER landed in cockroachdb-parser v0.26.2; v0.26.1
// rejects it with a SQLSTATE 42601 syntax error. If a future bump
// makes both versions accept the statement, this test goes
// silently green and loses its meaning — replace the statement
// with one that diverges between LATEST_QUARTER and the oldest
// supported sibling.
const crossVersionDivergentStmt = "ALTER TABLE t ENABLE TRIGGER tr"

func TestCrossVersionParserRouting(t *testing.T) {
	repoRoot := findRepoRoot(t)
	binDir := t.TempDir()
	latest := filepath.Join(binDir, "crdb-sql")
	sibling := filepath.Join(binDir, "crdb-sql-v261")

	buildCrdbSQL(t, repoRoot, latest, "v262", "")
	buildCrdbSQL(t, repoRoot, sibling, "v261", "build/go.v261.mod")

	pathWithBin := binDir + string(os.PathListSeparator) + os.Getenv("PATH")

	tests := []struct {
		name             string
		targetFlag       []string
		expectedErr      string
		expectedContains string
	}{
		{
			name: "latest accepts v26.2-only syntax",
			// Asserting on output (not just exit=0) catches a regression
			// where `parse` becomes a silent no-op exit-0 — the grammar
			// must actually have produced an AST.
			expectedContains: "ALTER TABLE",
		},
		{
			name:        "v26.1 sibling rejects v26.2-only syntax",
			targetFlag:  []string{"--target-version", "26.1.0"},
			expectedErr: "syntax error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{}, tc.targetFlag...)
			args = append(args, "parse", "-e", crossVersionDivergentStmt)
			cmd := exec.Command(latest, args...)
			cmd.Env = append(os.Environ(), "PATH="+pathWithBin)
			out, err := cmd.CombinedOutput()
			if tc.expectedErr != "" {
				require.Errorf(t, err, "expected non-zero exit, output: %s", out)
				require.Contains(t, string(out), tc.expectedErr)
				return
			}
			require.NoErrorf(t, err, "output: %s", out)
			require.Contains(t, string(out), tc.expectedContains,
				"latest binary parsed silently — expected AST output")
		})
	}
}

// findRepoRoot returns the directory containing the worktree's
// go.mod. Uses `go env GOMOD` so it works regardless of which
// package directory the test was invoked from.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").Output()
	require.NoError(t, err)
	gomod := strings.TrimSpace(string(out))
	require.NotEmpty(t, gomod, "go env GOMOD returned empty; not inside a module?")
	return filepath.Dir(gomod)
}

// buildCrdbSQL compiles the crdb-sql binary at outputPath, stamping
// the supplied quarter into versionroute.builtQuarterStamp. When
// modfile is non-empty, builds against build/<modfile> (the
// per-quarter sibling's go.mod replace directive); otherwise uses
// the worktree's top-level go.mod (the latest quarter).
func buildCrdbSQL(t *testing.T, repoRoot, outputPath, quarter, modfile string) {
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
