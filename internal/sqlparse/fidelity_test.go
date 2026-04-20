// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package sqlparse_test holds the fidelity test suite that asserts a
// curated corpus of canonical CockroachDB SQL parses cleanly with the
// vendored cockroachdb-parser. It exists as a test-only package; the
// production sqlparse API will be defined by whichever later issue
// (validate / format / extract) first needs a wrapper.
//
// Corpus files live under testdata/corpus/*.sql and may contain
// multiple statements separated by ';'. A file may begin with an
// optional pragma:
//
//	-- minparser: vMAJOR.MINOR.PATCH
//
// When present, the test skips the file unless currentParserVersion
// is >= the pragma's version. This lets us land coverage for SQL
// introduced in future parser releases without breaking older builds.
package sqlparse_test

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"golang.org/x/mod/semver"
)

// currentParserVersion is the cockroachdb-parser module version this
// build is pinned against. It must be kept in lockstep with the
// `replace` directive in go.mod (currently
// github.com/spilchen/cockroachdb-parser v0.26.2).
//
// It is hardcoded rather than read from debug.ReadBuildInfo because
// `go test` binaries do not populate BuildInfo.Deps — the same
// constraint observed by the version subcommand (see
// cmd/version.go). Bump this constant in the same commit that bumps
// the replace directive.
const currentParserVersion = "v0.26.2"

// corpusDir is resolved relative to the package directory; `go test`
// runs with CWD set to the package, which makes this the conventional
// Go testdata layout.
const corpusDir = "testdata/corpus"

// minParserPragma is the leading-comment marker that gates a corpus
// file on a minimum parser version. The parser is whitespace-
// tolerant; anything not matching is treated as a regular comment.
const minParserPragma = "-- minparser:"

func TestFidelity(t *testing.T) {
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
			if minVer != "" && semver.Compare(currentParserVersion, minVer) < 0 {
				t.Skipf("requires parser %s, have %s", minVer, currentParserVersion)
			}

			stmts, err := parser.Parse(string(data))
			if err != nil {
				t.Fatalf("parse %q: %v", path, err)
			}
			if len(stmts) == 0 {
				t.Fatalf("%q parsed to zero statements; corpus files must contain at least one statement", path)
			}
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
