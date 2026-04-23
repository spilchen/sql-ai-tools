// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package versionroute

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// makeFakeBackend creates a minimal executable file at dir/name. On
// Unix the file is given mode 0755 so isExecutable returns true; on
// Windows the .exe suffix is what isExecutable keys on.
func makeFakeBackend(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte{}, 0o755))
	return path
}

func TestFindBackendInPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("0o755 perm bits do not gate executability on Windows; covered indirectly by TestDiscover")
	}
	dir := t.TempDir()
	makeFakeBackend(t, dir, "crdb-sql-v254")
	t.Setenv("PATH", dir)

	path, ok := FindBackend(Quarter{Year: 25, Q: 4})
	require.True(t, ok)
	require.Equal(t, filepath.Join(dir, "crdb-sql-v254"), path)

	_, ok = FindBackend(Quarter{Year: 26, Q: 1})
	require.False(t, ok, "v261 was not installed")
}

func TestFindBackendIgnoresDirectories(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "crdb-sql-v254"), 0o755))
	t.Setenv("PATH", dir)

	_, ok := FindBackend(Quarter{Year: 25, Q: 4})
	require.False(t, ok, "directory with backend name must not match")
}

func TestDiscoverEnumeratesAndSorts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("see TestFindBackendInPath")
	}
	dir := t.TempDir()
	makeFakeBackend(t, dir, "crdb-sql-v251")
	makeFakeBackend(t, dir, "crdb-sql-v262")
	makeFakeBackend(t, dir, "crdb-sql-v254")
	makeFakeBackend(t, dir, "crdb-sql")      // no -vXXX suffix; ignored
	makeFakeBackend(t, dir, "crdb-sql-v999") // Q=9 invalid; ignored
	makeFakeBackend(t, dir, "other-binary")  // wrong prefix; ignored
	t.Setenv("PATH", dir)

	got := Discover()

	// The running test binary appears as IsSelf with an unknown
	// quarter (no parser dep in `go test` BuildInfo), so filter to
	// the discovered backends only for the quarter assertion.
	var quarters []Quarter
	for _, b := range got {
		if !b.IsSelf {
			quarters = append(quarters, b.Quarter)
		}
	}
	require.Equal(t, []Quarter{
		{Year: 26, Q: 2},
		{Year: 25, Q: 4},
		{Year: 25, Q: 1},
	}, quarters, "newest first")
}

func TestDiscoverDeduplicatesByPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("see TestFindBackendInPath")
	}
	dir := t.TempDir()
	makeFakeBackend(t, dir, "crdb-sql-v254")
	// Place the same directory in PATH twice; the same backend must
	// not appear twice in the result.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+dir)

	got := Discover()
	count := 0
	for _, b := range got {
		if b.Quarter == (Quarter{Year: 25, Q: 4}) && !b.IsSelf {
			count++
		}
	}
	require.Equal(t, 1, count)
}

func TestDiscoverIncludesSelf(t *testing.T) {
	t.Setenv("PATH", "")
	got := Discover()
	require.NotEmpty(t, got)
	require.True(t, got[len(got)-1].IsSelf || got[0].IsSelf,
		"self entry must be present somewhere in the result")
}

func TestDiscoverShadowsSameQuarterAcrossDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("see TestFindBackendInPath")
	}
	// Two PATH dirs both contain crdb-sql-v254. Discover must list
	// only one entry — the first by search order — mirroring how
	// shells resolve duplicate command names. A regression that drops
	// the seenQuarters dedup would surface as two v254 entries.
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	makeFakeBackend(t, dir1, "crdb-sql-v254")
	makeFakeBackend(t, dir2, "crdb-sql-v254")
	t.Setenv("PATH", dir1+string(os.PathListSeparator)+dir2)

	got := Discover()
	var v254s []Backend
	for _, b := range got {
		if b.Quarter == (Quarter{Year: 25, Q: 4}) && !b.IsSelf {
			v254s = append(v254s, b)
		}
	}
	require.Len(t, v254s, 1, "second sibling for the same quarter must be shadowed")
	require.Equal(t, filepath.Join(dir1, "crdb-sql-v254"), v254s[0].Path,
		"first by search order wins")
}
