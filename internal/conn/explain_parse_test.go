// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package conn

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParseExplainTree exercises the EXPLAIN tree parser against
// representative shapes produced by CockroachDB's default EXPLAIN: the
// degenerate empty result, header-only output, single-operator plans,
// nested operators connected via `└──`, and multi-child join shapes
// connected via `├──`/`└──`.
func TestParseExplainTree(t *testing.T) {
	tests := []struct {
		name           string
		rows           []string
		expectedHeader map[string]string
		expectedPlan   []PlanNode
	}{
		{
			name:           "empty rows",
			rows:           nil,
			expectedHeader: nil,
			expectedPlan:   nil,
		},
		{
			name: "header only",
			rows: []string{
				"distribution: local",
				"vectorized: true",
			},
			expectedHeader: map[string]string{
				"distribution": "local",
				"vectorized":   "true",
			},
			expectedPlan: nil,
		},
		{
			name: "single operator no attrs",
			rows: []string{
				"distribution: local",
				"vectorized: true",
				"·",
				"• values",
			},
			expectedHeader: map[string]string{"distribution": "local", "vectorized": "true"},
			expectedPlan:   []PlanNode{{Op: "values"}},
		},
		{
			name: "single operator with pipe-aligned attrs",
			rows: []string{
				"• scan",
				"│ table: t@primary",
				"│ spans: FULL SCAN",
			},
			expectedPlan: []PlanNode{
				{
					Op: "scan",
					Attrs: map[string]string{
						"table": "t@primary",
						"spans": "FULL SCAN",
					},
				},
			},
		},
		{
			name: "two-level nest with last-child connector",
			rows: []string{
				"• render",
				"│ estimated row count: 7",
				"│",
				"└── • scan",
				"      table: t@primary",
				"      spans: FULL SCAN",
			},
			expectedPlan: []PlanNode{
				{
					Op:    "render",
					Attrs: map[string]string{"estimated row count": "7"},
					Children: []PlanNode{
						{
							Op: "scan",
							Attrs: map[string]string{
								"table": "t@primary",
								"spans": "FULL SCAN",
							},
						},
					},
				},
			},
		},
		{
			name: "filter wrapping scan",
			rows: []string{
				"distribution: local",
				"vectorized: true",
				"·",
				"• filter",
				"│ filter: a > 5",
				"│",
				"└── • scan",
				"      table: t@primary",
				"      spans: FULL SCAN",
			},
			expectedHeader: map[string]string{"distribution": "local", "vectorized": "true"},
			expectedPlan: []PlanNode{
				{
					Op:    "filter",
					Attrs: map[string]string{"filter": "a > 5"},
					Children: []PlanNode{
						{
							Op: "scan",
							Attrs: map[string]string{
								"table": "t@primary",
								"spans": "FULL SCAN",
							},
						},
					},
				},
			},
		},
		{
			name: "two siblings under join",
			rows: []string{
				"• hash join",
				"│ equality: (a) = (b)",
				"│",
				"├── • scan",
				"│     table: t1@primary",
				"│",
				"└── • scan",
				"      table: t2@primary",
			},
			expectedPlan: []PlanNode{
				{
					Op:    "hash join",
					Attrs: map[string]string{"equality": "(a) = (b)"},
					Children: []PlanNode{
						{Op: "scan", Attrs: map[string]string{"table": "t1@primary"}},
						{Op: "scan", Attrs: map[string]string{"table": "t2@primary"}},
					},
				},
			},
		},
		{
			// Locks in the column-based ownership rule: the `│` glyphs
			// at column 0 of the t1 child's attribute rows belong to
			// the still-open join above, not to the t1 scan. A naïve
			// "owner is the most recently opened operator" rule would
			// pass the simpler one-attr-per-child shape but mis-route
			// here.
			name: "non-last child with multiple attributes preserves owner-by-column",
			rows: []string{
				"• hash join",
				"│ equality: (a) = (b)",
				"│",
				"├── • scan",
				"│     table: t1@primary",
				"│     spans: FULL SCAN",
				"│     estimated row count: 1000",
				"│",
				"└── • scan",
				"      table: t2@primary",
				"      spans: FULL SCAN",
			},
			expectedPlan: []PlanNode{
				{
					Op:    "hash join",
					Attrs: map[string]string{"equality": "(a) = (b)"},
					Children: []PlanNode{
						{
							Op: "scan",
							Attrs: map[string]string{
								"table":               "t1@primary",
								"spans":               "FULL SCAN",
								"estimated row count": "1000",
							},
						},
						{
							Op: "scan",
							Attrs: map[string]string{
								"table": "t2@primary",
								"spans": "FULL SCAN",
							},
						},
					},
				},
			},
		},
		{
			name: "attribute value containing colon",
			rows: []string{
				"• scan",
				"│ spans: /1/foo:bar",
			},
			expectedPlan: []PlanNode{
				{Op: "scan", Attrs: map[string]string{"spans": "/1/foo:bar"}},
			},
		},
		{
			name: "three-level nest",
			rows: []string{
				"• limit",
				"│ count: 10",
				"│",
				"└── • sort",
				"    │ order: +a",
				"    │",
				"    └── • scan",
				"          table: t@primary",
			},
			expectedPlan: []PlanNode{
				{
					Op:    "limit",
					Attrs: map[string]string{"count": "10"},
					Children: []PlanNode{
						{
							Op:    "sort",
							Attrs: map[string]string{"order": "+a"},
							Children: []PlanNode{
								{Op: "scan", Attrs: map[string]string{"table": "t@primary"}},
							},
						},
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			header, plan, err := parseExplainTree(tc.rows)
			require.NoError(t, err)
			require.Equal(t, tc.expectedHeader, header)
			require.Equal(t, tc.expectedPlan, plan)
		})
	}
}

// TestParseExplainTreeErrors covers the loud-failure contract: rows
// that don't fit the operator/attribute/separator shape return an error
// rather than being silently dropped.
func TestParseExplainTreeErrors(t *testing.T) {
	tests := []struct {
		name        string
		rows        []string
		expectedErr string
	}{
		{
			name:        "bullet without operator name",
			rows:        []string{"•   "},
			expectedErr: "bullet without operator name",
		},
		{
			name:        "header line missing colon",
			rows:        []string{"not a header"},
			expectedErr: "header line is not key: value",
		},
		{
			name: "orphan attribute after separator",
			rows: []string{
				"·",
				"  table: t@primary",
			},
			expectedErr: "has no open operator at column 0",
		},
		{
			name: "attribute over-indented relative to last operator",
			rows: []string{
				"• scan",
				"      │ table: t@primary",
			},
			expectedErr: "has no open operator at column 6",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parseExplainTree(tc.rows)
			require.Error(t, err)
			require.ErrorContains(t, err, tc.expectedErr)
		})
	}
}
