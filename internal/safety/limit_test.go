// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package safety_test

import (
	"strings"
	"testing"

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

func TestMaybeInjectLimitParseError(t *testing.T) {
	// Parser errors propagate unchanged so callers skipping safety.Check
	// don't see silent no-ops.
	out, injected, err := safety.MaybeInjectLimit("SELEKT broken", 1000)
	require.Error(t, err)
	require.False(t, injected)
	require.Equal(t, "SELEKT broken", out)
}
