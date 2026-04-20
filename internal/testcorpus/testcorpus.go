// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package testcorpus provides shared infrastructure for fidelity tests
// that run the curated SQL corpus through various parser and formatter
// APIs. It owns the parser-version constant, the `-- minparser:`
// pragma extraction, and the corpus-enumeration loop so that each
// fidelity test only needs to supply its assertion callback.
package testcorpus

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/semver"
)

// CurrentParserVersion is the cockroachdb-parser module version this
// build is pinned against. It must be kept in lockstep with the
// `replace` directive in go.mod (currently
// github.com/spilchen/cockroachdb-parser v0.26.2).
//
// It is hardcoded rather than read from debug.ReadBuildInfo because
// `go test` binaries do not populate BuildInfo.Deps — the same
// constraint observed by the version subcommand (see
// cmd/version.go). Bump this constant in the same commit that bumps
// the replace directive.
const CurrentParserVersion = "v0.26.2"

// minParserPragma is the leading-comment marker that gates a corpus
// file on a minimum parser version. The pragma extraction trims
// whitespace after the colon, so both `-- minparser: v0.26.2` and
// `-- minparser:v0.26.2` are accepted.
const minParserPragma = "-- minparser:"

// ForEachFile enumerates .sql files in corpusDir, creating a subtest
// per file. For each file it extracts the optional `-- minparser:`
// pragma and skips the subtest when CurrentParserVersion is below the
// required minimum. The file's content is passed to fn as a string.
//
// corpusDir is resolved relative to the calling package's directory
// since `go test` sets CWD to the package under test.
func ForEachFile(t *testing.T, corpusDir string, fn func(t *testing.T, sql string)) {
	t.Helper()

	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatalf("read corpus dir %q: %v", corpusDir, err)
	}

	var sqlFiles []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		sqlFiles = append(sqlFiles, e.Name())
	}
	if len(sqlFiles) == 0 {
		t.Fatalf("no .sql files found under %q; corpus must not be empty", corpusDir)
	}

	for _, name := range sqlFiles {
		path := filepath.Join(corpusDir, name)
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %q: %v", path, err)
			}

			minVer, err := extractMinParserPragma(string(data))
			if err != nil {
				t.Fatalf("invalid pragma in %q: %v", path, err)
			}
			if minVer != "" && semver.Compare(CurrentParserVersion, minVer) < 0 {
				t.Skipf("requires parser %s, have %s", minVer, CurrentParserVersion)
			}

			fn(t, string(data))
		})
	}
}

// extractMinParserPragma scans the leading `--` comment block of a
// corpus file for a `-- minparser: vX.Y.Z` line and returns the
// version string. It returns ("", nil) when no pragma is present
// (the common case). A pragma whose value fails semver.IsValid is
// reported as an error so typos surface loudly rather than silently
// widening coverage.
//
// Only leading comments are inspected: scanning stops at the first
// line that is neither blank nor a `--` comment. A pragma buried
// inside the SQL body is intentionally ignored.
func extractMinParserPragma(content string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "--") {
			return "", nil
		}
		if !strings.HasPrefix(line, minParserPragma) {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, minParserPragma))
		if !semver.IsValid(v) {
			return "", &pragmaError{value: v}
		}
		return v, nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", nil
}

// pragmaError reports a malformed minparser pragma value. It carries
// the offending string so the test failure tells the author exactly
// what to fix.
type pragmaError struct {
	value string
}

func (e *pragmaError) Error() string {
	return "minparser pragma value is not valid semver: " + e.value
}
