// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeFile creates a file with the given contents under dir, including
// any parent directories. Test helper only.
func writeFile(t *testing.T, dir, rel, contents string) string {
	t.Helper()
	full := filepath.Join(dir, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(contents), 0o644))
	return full
}

// TestLoadValid covers the happy path: a well-formed config parses
// into the expected structure with BaseDir populated.
func TestLoadValid(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "crdb-sql.yaml", `version: 1
sql:
  - schema: ["schema/*.sql"]
    queries: ["queries/**/*.sql"]
  - schema: ["test/schema.sql"]
    queries: ["test/**/*.sql"]
`)

	f, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, 1, f.Version)
	require.Len(t, f.SQL, 2)
	require.Equal(t, []string{"schema/*.sql"}, f.SQL[0].Schema)
	require.Equal(t, []string{"queries/**/*.sql"}, f.SQL[0].Queries)
	require.Equal(t, dir, f.BaseDir)
}

// TestLoadErrors exercises the rejection paths so that misconfigured
// files fail loudly at load time rather than producing surprising
// empty matches downstream.
func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name        string
		contents    string
		expectedErr string
	}{
		{
			name: "unsupported version",
			contents: `version: 99
sql: []
`,
			expectedErr: "unsupported version 99",
		},
		{
			name: "missing version defaults to zero and rejected",
			contents: `sql:
  - schema: ["a.sql"]
    queries: ["q.sql"]
`,
			expectedErr: "unsupported version 0",
		},
		{
			name: "unknown top-level field",
			contents: `version: 1
sql: []
unexpected: true
`,
			expectedErr: "field unexpected not found",
		},
		{
			name: "typo in nested field",
			contents: `version: 1
sql:
  - schema: ["a.sql"]
    querys: ["q.sql"]
`,
			expectedErr: "field querys not found",
		},
		{
			name:        "malformed yaml",
			contents:    "version: 1\nsql: [unterminated",
			expectedErr: "parse config file",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeFile(t, dir, "crdb-sql.yaml", tc.contents)
			_, err := Load(path)
			require.Error(t, err)
			require.ErrorContains(t, err, tc.expectedErr)
		})
	}
}

// TestLoadMissingFile verifies that Load (unlike Discover) treats a
// missing file as an error — explicit paths must exist.
func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	require.Error(t, err)
}

// TestDiscoverMissing verifies that absence of any default-named
// config returns (nil, nil). This is what lets every command tolerate
// being run outside a configured project.
func TestDiscoverMissing(t *testing.T) {
	f, err := Discover(t.TempDir())
	require.NoError(t, err)
	require.Nil(t, f)
}

// TestDiscoverPicksFirstMatch verifies that crdb-sql.yaml takes
// precedence over crdb-sql.yml when both are present.
func TestDiscoverPicksFirstMatch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "crdb-sql.yaml", "version: 1\nsql: []\n")
	writeFile(t, dir, "crdb-sql.yml", "version: 99\nsql: []\n")

	f, err := Discover(dir)
	require.NoError(t, err)
	require.NotNil(t, f)
	require.Equal(t, 1, f.Version, "should have loaded the .yaml file, not the .yml")
}

// TestDiscoverYmlSpelling verifies that the .yml fallback works when
// only that spelling is present.
func TestDiscoverYmlSpelling(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "crdb-sql.yml", "version: 1\nsql: []\n")

	f, err := Discover(dir)
	require.NoError(t, err)
	require.NotNil(t, f)
	require.Equal(t, dir, f.BaseDir)
}

// TestExpandGlobs covers schema/query glob resolution. The cases
// exercise the dimensions that are easy to get wrong: deep recursion
// via `**`, deduplication when patterns overlap, deterministic
// ordering, and zero-match patterns.
func TestExpandGlobs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "schema/users.sql", "")
	writeFile(t, dir, "schema/orders.sql", "")
	writeFile(t, dir, "queries/q1.sql", "")
	writeFile(t, dir, "queries/sub/q2.sql", "")
	writeFile(t, dir, "queries/sub/deep/q3.sql", "")

	tests := []struct {
		name             string
		patterns         []string
		expectedSuffixes []string
	}{
		{
			name:     "no patterns returns nil",
			patterns: nil,
		},
		{
			name:     "single shallow glob",
			patterns: []string{"schema/*.sql"},
			expectedSuffixes: []string{
				"schema/orders.sql",
				"schema/users.sql",
			},
		},
		{
			name:     "double-star recursive glob",
			patterns: []string{"queries/**/*.sql"},
			expectedSuffixes: []string{
				"queries/q1.sql",
				"queries/sub/deep/q3.sql",
				"queries/sub/q2.sql",
			},
		},
		{
			name:     "overlapping patterns deduplicate",
			patterns: []string{"queries/**/*.sql", "queries/sub/*.sql"},
			expectedSuffixes: []string{
				"queries/q1.sql",
				"queries/sub/deep/q3.sql",
				"queries/sub/q2.sql",
			},
		},
		{
			name:     "no matches returns empty without error",
			patterns: []string{"missing/*.sql"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := expandGlobs(dir, tc.patterns)
			require.NoError(t, err)
			require.Len(t, got, len(tc.expectedSuffixes))
			for i, suf := range tc.expectedSuffixes {
				require.True(t, strings.HasSuffix(got[i], suf),
					"result %d %q should end with %q", i, got[i], suf)
			}
		})
	}
}

// TestExpandGlobsAbsolutePath verifies that an absolute pattern is
// honored as-is and not re-rooted under baseDir.
func TestExpandGlobsAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "abs.sql", "")
	abs := filepath.Join(dir, "abs.sql")

	got, err := expandGlobs("/some/other/place", []string{abs})
	require.NoError(t, err)
	require.Equal(t, []string{abs}, got)
}

// TestSQLPairExpand verifies the public ExpandSchema/ExpandQueries
// methods route through the same machinery as expandGlobs.
func TestSQLPairExpand(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "s.sql", "")
	writeFile(t, dir, "q.sql", "")

	p := SQLPair{
		Schema:  []string{"s.sql"},
		Queries: []string{"q.sql"},
	}
	schema, err := p.ExpandSchema(dir)
	require.NoError(t, err)
	require.Len(t, schema, 1)
	require.True(t, strings.HasSuffix(schema[0], "s.sql"))

	queries, err := p.ExpandQueries(dir)
	require.NoError(t, err)
	require.Len(t, queries, 1)
	require.True(t, strings.HasSuffix(queries[0], "q.sql"))
}
