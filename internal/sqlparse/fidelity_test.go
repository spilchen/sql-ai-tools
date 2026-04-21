// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package sqlparse_test holds the fidelity test suite that asserts a
// curated corpus of canonical CockroachDB SQL parses cleanly with the
// vendored cockroachdb-parser.
//
// Corpus files live under testdata/corpus/*.sql and may contain
// multiple statements separated by ';'. A file may begin with an
// optional pragma:
//
//	-- minparser: vMAJOR.MINOR.PATCH
//
// When present, the test skips the file unless the pinned parser
// version is >= the pragma's version. This lets us land coverage for
// SQL introduced in future parser releases without breaking older
// builds. See package testcorpus for the pragma implementation.
package sqlparse_test

import (
	"testing"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/tree"
	"github.com/spilchen/sql-ai-tools/internal/sqlparse"
	"github.com/spilchen/sql-ai-tools/internal/testcorpus"
	"github.com/stretchr/testify/require"
)

// corpusDir is resolved relative to the package directory; `go test`
// runs with CWD set to the package, which makes this the conventional
// Go testdata layout.
const corpusDir = "testdata/corpus"

func TestFidelity(t *testing.T) {
	testcorpus.ForEachFile(t, corpusDir, func(t *testing.T, sql string) {
		stmts, err := parser.Parse(sql)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(stmts) == 0 {
			t.Fatal("parsed to zero statements; corpus files must contain at least one statement")
		}

		for i, stmt := range stmts {
			normalized := tree.FormatStatementHideConstants(stmt.AST)
			if normalized == "" {
				t.Errorf("stmt[%d]: FormatStatementHideConstants returned empty string", i)
				continue
			}
			if _, err := parser.Parse(normalized); err != nil {
				t.Errorf("stmt[%d]: normalized form does not re-parse: %v\nnormalized: %s", i, err, normalized)
			}
		}
	})
}

// TestFidelityClassify verifies that every corpus file classifies
// cleanly through sqlparse.Classify and that each resulting statement
// carries a non-empty StatementType, Tag, SQL, and Normalized field. This
// complements TestFidelity, which only asserts that the raw parser
// accepts the corpus without error.
func TestFidelityClassify(t *testing.T) {
	testcorpus.ForEachFile(t, corpusDir, func(t *testing.T, sql string) {
		stmts, err := sqlparse.Classify(sql)
		require.NoError(t, err)
		require.NotEmpty(t, stmts)

		for i, stmt := range stmts {
			require.NotEmpty(t, stmt.StatementType, "statement %d has empty StatementType", i)
			require.NotEmpty(t, stmt.Tag, "statement %d has empty Tag", i)
			require.NotEmpty(t, stmt.SQL, "statement %d has empty SQL", i)
			require.NotEmpty(t, stmt.Normalized, "statement %d has empty Normalized", i)
		}
	})
}
