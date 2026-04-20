// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// This file holds the fidelity test that asserts the curated corpus
// of canonical CockroachDB SQL validates without error through the
// full validate command pipeline. Unlike TestFidelity in sqlparse
// (which exercises the raw parser), this test covers the CLI command
// stack — input handling, parsing, rendering — so that future validate
// enhancements (expression type checking, name resolution) are
// automatically tested against the corpus.
package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spilchen/sql-ai-tools/internal/testcorpus"
	"github.com/stretchr/testify/require"
)

const fidelityCorpusDir = "../internal/sqlparse/testdata/corpus"

// TestFidelityValidate runs each corpus file through the validate
// command and asserts that no errors are reported.
func TestFidelityValidate(t *testing.T) {
	testcorpus.ForEachFile(t, fidelityCorpusDir, func(t *testing.T, sql string) {
		root := newRootCmd()
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		root.SetIn(strings.NewReader(sql))
		root.SetArgs([]string{"validate"})

		require.NoError(t, root.Execute())
	})
}
