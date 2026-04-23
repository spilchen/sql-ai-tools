// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package versionroute

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Backend describes one discovered crdb-sql backend on the host. It is
// returned by Discover and consumed by both the routing logic
// (FindBackend) and the `versions` subcommand.
type Backend struct {
	// Quarter is the parsed Year.Quarter the backend is built against.
	// May be the zero value for the running binary when no stamp and
	// no parser dep are available; sibling entries always have a
	// valid Quarter (their filename was the source of truth).
	Quarter Quarter
	// Path is the absolute filesystem path to the binary. Always
	// populated for entries that appear in Discover's output —
	// selfBackend skips the running binary entirely if os.Executable
	// fails, so a Backend with an empty Path is never produced.
	Path string
	// IsSelf is true for the entry that represents the currently
	// running binary. The discovery walk skips Path-equal duplicates
	// elsewhere in the search order so each backend appears at most
	// once per Discover call.
	IsSelf bool
}

// FindBackend locates a sibling backend matching want. The search
// order matches Discover (executable's directory first, then $PATH);
// the first match wins. Returns ("", false) when no match exists.
//
// Callers in MaybeReexec use the returned path to syscall.Exec (or
// the Windows equivalent). The path is always absolute when found.
func FindBackend(want Quarter) (string, bool) {
	name := want.BackendName() + execSuffix()
	if dir := selfDir(); dir != "" {
		candidate := filepath.Join(dir, name)
		if isExecutable(candidate) {
			return candidate, true
		}
	}
	for _, dir := range pathDirs() {
		candidate := filepath.Join(dir, name)
		if isExecutable(candidate) {
			return candidate, true
		}
	}
	return "", false
}

// Discover enumerates every crdb-sql-vXXX backend reachable from the
// current process: the running binary itself, any sibling alongside
// it, and any matching binaries on $PATH. Results are sorted by
// Quarter (newest first) so the `versions` subcommand can print them
// directly.
//
// Duplicates (same absolute path discovered via two search-path
// entries) are coalesced; the first occurrence wins. Sibling
// shadowing: if two siblings carry the same Quarter (e.g. one in
// $PATH and one alongside the binary), the first wins by search
// order, which mirrors how the OS resolves command names. The self
// entry claims its own Quarter the same way, so a sibling with the
// same Quarter as the running binary is suppressed.
//
// The running binary is included with IsSelf=true whenever
// os.Executable() succeeds (the common case on supported platforms).
// On the rare failure, no self entry is produced and the result
// contains only siblings; the missing-backend error path documents
// the expected non-empty case.
func Discover() []Backend {
	var out []Backend
	seenPaths := map[string]bool{}
	seenQuarters := map[Quarter]bool{}

	if self, ok := selfBackend(); ok {
		out = append(out, self)
		seenPaths[self.Path] = true
		if !self.Quarter.IsZero() {
			seenQuarters[self.Quarter] = true
		}
	}

	addCandidate := func(path string) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return
		}
		if seenPaths[abs] {
			return
		}
		base := filepath.Base(abs)
		base = strings.TrimSuffix(base, execSuffix())
		// Backend filenames look like "crdb-sql-v254"; the prefix
		// strip yields the bare tag "v254" we hand to ParseTag.
		const prefix = "crdb-sql-"
		if !strings.HasPrefix(base, prefix) {
			return
		}
		q, ok := ParseTag(strings.TrimPrefix(base, prefix))
		if !ok {
			return
		}
		if seenQuarters[q] {
			// A different sibling for the same quarter shadows this
			// one (same way $PATH lookup works). Skip.
			return
		}
		if !isExecutable(abs) {
			return
		}
		seenPaths[abs] = true
		seenQuarters[q] = true
		out = append(out, Backend{Quarter: q, Path: abs})
	}

	if dir := selfDir(); dir != "" {
		walkBackendDir(dir, addCandidate)
	}
	for _, dir := range pathDirs() {
		walkBackendDir(dir, addCandidate)
	}

	// Sort by Quarter descending (newest first); the self entry
	// participates in this ordering — it should not unconditionally
	// lead the list, since users with multiple installed quarters
	// expect a chronological view.
	sort.SliceStable(out, func(i, j int) bool {
		return quarterLess(out[j].Quarter, out[i].Quarter)
	})
	return out
}

// quarterLess returns true if a sorts before b in chronological
// order. Year is the dominant key; quarter breaks ties. The zero
// Quarter sorts last so an unknown self entry does not crowd out
// real backends in the discovery output.
func quarterLess(a, b Quarter) bool {
	if a.IsZero() {
		return false
	}
	if b.IsZero() {
		return true
	}
	if a.Year != b.Year {
		return a.Year < b.Year
	}
	return a.Q < b.Q
}

// selfBackend constructs the Backend entry for the running process.
// Returns false only when os.Executable fails — extremely rare on
// supported platforms, but tolerated so discovery still functions.
func selfBackend() (Backend, bool) {
	exe, err := os.Executable()
	if err != nil {
		return Backend{}, false
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		abs = exe
	}
	q, _ := Built()
	return Backend{Quarter: q, Path: abs, IsSelf: true}, true
}

// selfDir is the directory containing the running executable, or "" if
// it cannot be resolved. Routing prefers this location over $PATH so a
// user-extracted release archive ("untar to /opt/crdb-sql") works
// without modifying $PATH.
func selfDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return ""
	}
	return filepath.Dir(abs)
}

// pathDirs returns the search directories from $PATH, in order. Empty
// entries (a leading or trailing colon) are skipped because filepath.Join
// would treat "" as the current directory and yield surprising matches.
func pathDirs() []string {
	raw := os.Getenv("PATH")
	if raw == "" {
		return nil
	}
	parts := filepath.SplitList(raw)
	out := parts[:0]
	for _, p := range parts {
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// walkBackendDir invokes cb for every regular file in dir whose name
// looks like a backend binary (crdb-sql-vXXX[.exe]). It does not
// recurse — release archives and standard install layouts put all
// siblings in one flat directory.
func walkBackendDir(dir string, cb func(path string)) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "crdb-sql-v") {
			continue
		}
		cb(filepath.Join(dir, name))
	}
}

// isExecutable returns true if path is a regular file that the OS
// would consider runnable. On Unix this checks the executable bit; on
// Windows it relies on the .exe suffix being present (Stat alone is
// enough since execSuffix() ensures we only ask about .exe files).
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0o111 != 0
}

// execSuffix is the platform's executable suffix (".exe" on Windows,
// "" elsewhere). Centralized so backend-name construction and
// extension-stripping during discovery stay symmetric.
func execSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
