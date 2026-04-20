// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package sqlformat_test holds the fidelity test that asserts the
// curated corpus of canonical CockroachDB SQL formats without error
// and that the formatted output preserves statement count and
// classification when re-parsed. See package testcorpus for the
// corpus enumeration and pragma infrastructure.
package sqlformat_test

import (
	"testing"

	"github.com/spilchen/sql-ai-tools/internal/sqlformat"
	"github.com/spilchen/sql-ai-tools/internal/sqlparse"
	"github.com/spilchen/sql-ai-tools/internal/testcorpus"
	"github.com/stretchr/testify/require"
)

// corpusDir points at the shared corpus owned by sqlparse; the
// relative path works because go test sets CWD to the package
// directory.
const corpusDir = "../sqlparse/testdata/corpus"

// TestFidelityFormat verifies that every corpus file formats without
// error and that the formatted output preserves statement count and
// classification when re-parsed through sqlparse.Classify.
func TestFidelityFormat(t *testing.T) {
	testcorpus.ForEachFile(t, corpusDir, func(t *testing.T, sql string) {
		formatted, err := sqlformat.Format(sql)
		require.NoError(t, err)
		require.NotEmpty(t, formatted)

		original, err := sqlparse.Classify(sql)
		require.NoError(t, err)

		roundTripped, err := sqlparse.Classify(formatted)
		require.NoError(t, err)
		require.Equal(t, len(original), len(roundTripped),
			"formatted output should produce same number of statements")

		for i := range original {
			require.Equal(t, original[i].StatementType, roundTripped[i].StatementType,
				"statement %d: StatementType changed after formatting", i)
			require.Equal(t, original[i].Tag, roundTripped[i].Tag,
				"statement %d: Tag changed after formatting", i)
		}
	})
}
