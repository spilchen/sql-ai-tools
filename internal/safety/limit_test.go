// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package safety_test

import (
	"strings"
	"testing"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/tree"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/safety"
)

func TestMaybeInjectLimit(t *testing.T) {
	tests := []struct {
		name                   string
		sql                    string
		max                    int
		expectedInjected       bool
		expectedContains       string
		expectedContainsOffset bool
	}{
		{
			name:             "bare select gets limit",
			sql:              "SELECT * FROM t",
			max:              1000,
			expectedInjected: true,
			expectedContains: "LIMIT 1000",
		},
		{
			name:             "select with where gets limit",
			sql:              "SELECT id FROM t WHERE id = 1",
			max:              50,
			expectedInjected: true,
			expectedContains: "LIMIT 50",
		},
		{
			name: "select with existing limit unchanged",
			sql:  "SELECT * FROM t LIMIT 5",
			max:  1000,
		},
		{
			name: "select with offset and existing limit unchanged",
			// Pin that the bounded check sees both flavours of "the
			// caller already capped it".
			sql: "SELECT * FROM t LIMIT 5 OFFSET 10",
			max: 1000,
		},
		{
			name: "select with limit all unchanged",
			// LIMIT ALL is the explicit "give me everything" knob;
			// honour it rather than overwriting with maxRows.
			sql: "SELECT * FROM t LIMIT ALL",
			max: 1000,
		},
		{
			name: "insert returning unchanged",
			sql:  "INSERT INTO t VALUES (1) RETURNING id",
			max:  1000,
		},
		{
			name: "delete unchanged",
			sql:  "DELETE FROM t WHERE id = 1",
			max:  1000,
		},
		{
			name: "ddl unchanged",
			sql:  "CREATE TABLE x (id INT PRIMARY KEY)",
			max:  1000,
		},
		{
			name: "multi-statement batch unchanged",
			sql:  "SELECT 1; SELECT 2",
			max:  1000,
		},
		{
			name: "max zero means unlimited",
			sql:  "SELECT * FROM t",
			max:  0,
		},
		{
			name: "negative max means unlimited",
			sql:  "SELECT * FROM t",
			max:  -1,
		},
		{
			name: "explain wrapper unchanged",
			sql:  "EXPLAIN SELECT * FROM t",
			max:  1000,
		},
		{
			name: "select with offset only gets limit injected",
			// SELECT … OFFSET N (no LIMIT) is still unbounded — the
			// parser stores OFFSET inside the same *Limit node as
			// Count, so a naive Limit==nil check would skip injection
			// and let an unbounded result stream through. The rewriter
			// looks at Count specifically and preserves the OFFSET.
			// Both LIMIT and OFFSET are asserted: a regression that
			// overwrote sel.Limit (instead of mutating Count) would
			// drop the OFFSET silently and still match a "LIMIT N"
			// substring check.
			sql:                    "SELECT * FROM t OFFSET 10",
			max:                    500,
			expectedInjected:       true,
			expectedContains:       "LIMIT 500",
			expectedContainsOffset: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, injected, err := safety.MaybeInjectLimit(tc.sql, tc.max)
			require.NoError(t, err)
			require.Equal(t, tc.expectedInjected, injected)
			if !tc.expectedInjected {
				require.Equal(t, tc.sql, out, "input must be returned verbatim when no injection")
				return
			}
			require.True(t, strings.Contains(strings.ToUpper(out), tc.expectedContains),
				"expected output %q to contain %q", out, tc.expectedContains)
			if tc.expectedContainsOffset {
				require.True(t, strings.Contains(strings.ToUpper(out), "OFFSET 10"),
					"expected output %q to preserve OFFSET 10 alongside the injected LIMIT", out)
			}
		})
	}
}

// TestMaybeInjectLimitParsed pins the parsed-input contract directly,
// rather than only through the string-input wrapper. The wrapper
// masks no-injection by returning the original SQL string, so a
// parsed-variant regression that returned ("", false) for an
// injectable bare SELECT would be invisible to wrapper-only tests
// (the wrapper would silently swap back to the original input and
// look like a deliberate no-op). Direct coverage of the *Parsed
// entry point keeps the empty-string-on-no-injection contract
// regression-tested.
func TestMaybeInjectLimitParsed(t *testing.T) {
	tests := []struct {
		name             string
		sql              string
		max              int
		expectedInjected bool
		expectedContains string
	}{
		{
			name:             "bare select gets limit",
			sql:              "SELECT * FROM t",
			max:              250,
			expectedInjected: true,
			expectedContains: "LIMIT 250",
		},
		{
			name: "non-select returns empty no-injection",
			sql:  "INSERT INTO t VALUES (1)",
			max:  100,
		},
		{
			name: "already-bounded select returns empty no-injection",
			sql:  "SELECT * FROM t LIMIT 5",
			max:  100,
		},
		{
			name: "max zero returns empty no-injection",
			sql:  "SELECT * FROM t",
			max:  0,
		},
		{
			name: "multi-statement returns empty no-injection",
			sql:  "SELECT 1; SELECT 2",
			max:  100,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stmts, err := parser.Parse(tc.sql)
			require.NoError(t, err)

			out, injected := safety.MaybeInjectLimitParsed(stmts, tc.max)
			require.Equal(t, tc.expectedInjected, injected)
			if !tc.expectedInjected {
				require.Empty(t, out,
					"no-injection contract: parsed variant returns empty string, not the original SQL")
				return
			}
			require.Contains(t, strings.ToUpper(out), tc.expectedContains)
		})
	}
}

// TestMaybeInjectLimitParsedMutatesAST pins the documented ownership
// contract: when injection fires, stmts[0].AST is mutated in place so
// downstream consumers of the same Statements slice see the new
// LIMIT. A regression that built a fresh AST and serialized it would
// pass the in/out string assertion but break the documented "captures
// stmts by reference" contract that exec.go and execute.go rely on.
func TestMaybeInjectLimitParsedMutatesAST(t *testing.T) {
	stmts, err := parser.Parse("SELECT * FROM t")
	require.NoError(t, err)

	sel, ok := stmts[0].AST.(*tree.Select)
	require.True(t, ok)
	require.Nil(t, sel.Limit, "precondition: bare SELECT has no Limit node")

	_, injected := safety.MaybeInjectLimitParsed(stmts, 42)
	require.True(t, injected)
	require.NotNil(t, sel.Limit, "AST must be mutated in place to reflect the injected LIMIT")
	require.NotNil(t, sel.Limit.Count)
}

func TestMaybeInjectLimitParseError(t *testing.T) {
	// Parser errors propagate unchanged so callers skipping safety.Check
	// don't see silent no-ops.
	out, injected, err := safety.MaybeInjectLimit("SELEKT broken", 1000)
	require.Error(t, err)
	require.False(t, injected)
	require.Equal(t, "SELEKT broken", out)
}
