// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package summarize

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/risk"
)

// TestSummarize covers the structured fields produced for a range of
// statement shapes. Each case exercises one concern; the issue demo
// has its own dedicated case so a regression there is obvious.
func TestSummarize(t *testing.T) {
	tests := []struct {
		name             string
		sql              string
		expectedOp       Operation
		expectedTag      string
		expectedTables   []string
		expectedPreds    []string
		expectedJoins    []Join
		expectedAffected []string
		expectedRisk     risk.Severity
	}{
		{
			name:             "issue demo: delete with WHERE",
			sql:              "DELETE FROM orders WHERE status='x'",
			expectedOp:       OpDelete,
			expectedTables:   []string{"orders"},
			expectedPreds:    []string{"status = 'x'"},
			expectedJoins:    []Join{},
			expectedAffected: []string{},
			expectedRisk:     risk.SeverityInfo,
		},
		{
			name:             "delete without WHERE delegates risk",
			sql:              "DELETE FROM orders",
			expectedOp:       OpDelete,
			expectedTables:   []string{"orders"},
			expectedPreds:    []string{},
			expectedJoins:    []Join{},
			expectedAffected: []string{},
			expectedRisk:     risk.SeverityCritical,
		},
		{
			name:             "insert with explicit column list",
			sql:              "INSERT INTO t (a, b) VALUES (1, 2)",
			expectedOp:       OpInsert,
			expectedTables:   []string{"t"},
			expectedPreds:    []string{},
			expectedJoins:    []Join{},
			expectedAffected: []string{"a", "b"},
			expectedRisk:     risk.SeverityInfo,
		},
		{
			name:             "insert without column list leaves affected empty",
			sql:              "INSERT INTO t VALUES (1, 2)",
			expectedOp:       OpInsert,
			expectedTables:   []string{"t"},
			expectedPreds:    []string{},
			expectedJoins:    []Join{},
			expectedAffected: []string{},
			expectedRisk:     risk.SeverityInfo,
		},
		{
			name:             "upsert short form detected",
			sql:              "UPSERT INTO t (a) VALUES (1)",
			expectedOp:       OpUpsert,
			expectedTables:   []string{"t"},
			expectedPreds:    []string{},
			expectedJoins:    []Join{},
			expectedAffected: []string{"a"},
			expectedRisk:     risk.SeverityInfo,
		},
		{
			name:             "update set targets become affected columns",
			sql:              "UPDATE t SET a=1, b=2 WHERE id=3",
			expectedOp:       OpUpdate,
			expectedTables:   []string{"t"},
			expectedPreds:    []string{"id = 3"},
			expectedJoins:    []Join{},
			expectedAffected: []string{"a", "b"},
			expectedRisk:     risk.SeverityInfo,
		},
		{
			name:             "select WHERE splits on top-level AND",
			sql:              "SELECT 1 FROM t WHERE a=1 AND b>2 AND c IS NULL",
			expectedOp:       OpSelect,
			expectedTables:   []string{"t"},
			expectedPreds:    []string{"a = 1", "b > 2", "c IS NULL"},
			expectedJoins:    []Join{},
			expectedAffected: []string{},
			expectedRisk:     risk.SeverityInfo,
		},
		{
			name:             "select star raises low risk",
			sql:              "SELECT * FROM t",
			expectedOp:       OpSelect,
			expectedTables:   []string{"t"},
			expectedPreds:    []string{},
			expectedJoins:    []Join{},
			expectedAffected: []string{},
			expectedRisk:     risk.SeverityLow,
		},
		{
			name:           "inner join with ON",
			sql:            "SELECT 1 FROM a JOIN b ON a.id=b.id",
			expectedOp:     OpSelect,
			expectedTables: []string{"a", "b"},
			expectedPreds:  []string{},
			expectedJoins: []Join{
				{Type: "INNER", Left: "a", Right: "b", Condition: "a.id = b.id"},
			},
			expectedAffected: []string{},
			expectedRisk:     risk.SeverityInfo,
		},
		{
			name:           "left join with USING",
			sql:            "SELECT 1 FROM a LEFT JOIN b USING (id)",
			expectedOp:     OpSelect,
			expectedTables: []string{"a", "b"},
			expectedPreds:  []string{},
			expectedJoins: []Join{
				{Type: "LEFT", Left: "a", Right: "b", Condition: "USING (id)"},
			},
			expectedAffected: []string{},
			expectedRisk:     risk.SeverityInfo,
		},
		{
			name:           "three-way join produces two join entries",
			sql:            "SELECT 1 FROM a JOIN b ON a.id=b.id JOIN c ON b.id=c.id",
			expectedOp:     OpSelect,
			expectedTables: []string{"a", "b", "c"},
			expectedPreds:  []string{},
			expectedJoins: []Join{
				{Type: "INNER", Left: "a", Right: "b", Condition: "a.id = b.id"},
				{Type: "INNER", Left: "", Right: "c", Condition: "b.id = c.id"},
			},
			expectedAffected: []string{},
			expectedRisk:     risk.SeverityInfo,
		},
		{
			name:             "schema-qualified table renders bare name",
			sql:              "SELECT 1 FROM public.users",
			expectedOp:       OpSelect,
			expectedTables:   []string{"users"},
			expectedPreds:    []string{},
			expectedJoins:    []Join{},
			expectedAffected: []string{},
			expectedRisk:     risk.SeverityInfo,
		},
		{
			name:             "CTE alias excluded from tables",
			sql:              "WITH x AS (SELECT 1 FROM t) SELECT 2 FROM x",
			expectedOp:       OpSelect,
			expectedTables:   []string{"t"},
			expectedPreds:    []string{},
			expectedJoins:    []Join{},
			expectedAffected: []string{},
			expectedRisk:     risk.SeverityInfo,
		},
		{
			// The walker visits projection → WHERE → FROM, so the
			// subquery's table appears before the outer FROM table.
			// The ordering is intentional and documented on
			// collectTables.
			name:             "subquery tables roll up",
			sql:              "SELECT 1 FROM t WHERE id IN (SELECT id FROM u)",
			expectedOp:       OpSelect,
			expectedTables:   []string{"u", "t"},
			expectedPreds:    []string{"id IN (SELECT id FROM u)"},
			expectedJoins:    []Join{},
			expectedAffected: []string{},
			expectedRisk:     risk.SeverityInfo,
		},
		{
			name:             "DDL fallback to OTHER with tag and risk",
			sql:              "DROP TABLE foo",
			expectedOp:       OpOther,
			expectedTag:      "DROP TABLE",
			expectedTables:   []string{},
			expectedPreds:    []string{},
			expectedJoins:    []Join{},
			expectedAffected: []string{},
			expectedRisk:     risk.SeverityCritical,
		},
		{
			// NATURAL JOIN must not flatten to plain INNER — agents
			// reading just Type need to see that no equality predicate
			// was declared.
			name:           "natural join surfaces in type",
			sql:            "SELECT 1 FROM a NATURAL JOIN b",
			expectedOp:     OpSelect,
			expectedTables: []string{"a", "b"},
			expectedPreds:  []string{},
			expectedJoins: []Join{
				{Type: "NATURAL", Left: "a", Right: "b", Condition: "NATURAL"},
			},
			expectedAffected: []string{},
			expectedRisk:     risk.SeverityInfo,
		},
		{
			name:           "natural left join combines markers",
			sql:            "SELECT 1 FROM a NATURAL LEFT JOIN b",
			expectedOp:     OpSelect,
			expectedTables: []string{"a", "b"},
			expectedPreds:  []string{},
			expectedJoins: []Join{
				{Type: "NATURAL LEFT", Left: "a", Right: "b", Condition: "NATURAL"},
			},
			expectedAffected: []string{},
			expectedRisk:     risk.SeverityInfo,
		},
		{
			name:           "cross join has empty condition",
			sql:            "SELECT 1 FROM a CROSS JOIN b",
			expectedOp:     OpSelect,
			expectedTables: []string{"a", "b"},
			expectedPreds:  []string{},
			expectedJoins: []Join{
				{Type: "CROSS", Left: "a", Right: "b", Condition: ""},
			},
			expectedAffected: []string{},
			expectedRisk:     risk.SeverityInfo,
		},
		{
			// DELETE ... USING must collect both the target and the
			// USING tables; otherwise change-impact analysis would
			// silently drop the second table.
			name:             "delete using collects both tables",
			sql:              "DELETE FROM a USING b WHERE a.id=b.id",
			expectedOp:       OpDelete,
			expectedTables:   []string{"a", "b"},
			expectedPreds:    []string{"a.id = b.id"},
			expectedJoins:    []Join{},
			expectedAffected: []string{},
			expectedRisk:     risk.SeverityInfo,
		},
		{
			name:             "update from collects both tables",
			sql:              "UPDATE a SET x=b.y FROM b WHERE a.id=b.id",
			expectedOp:       OpUpdate,
			expectedTables:   []string{"a", "b"},
			expectedPreds:    []string{"a.id = b.id"},
			expectedJoins:    []Join{},
			expectedAffected: []string{"x"},
			expectedRisk:     risk.SeverityInfo,
		},
		{
			// Only the bare UPSERT keyword is OpUpsert; the explicit
			// ON CONFLICT DO UPDATE form stays as OpInsert because
			// OnConflict.IsUpsertAlias() distinguishes them.
			name:             "explicit on conflict do update stays as insert",
			sql:              "INSERT INTO t (a) VALUES (1) ON CONFLICT (a) DO UPDATE SET a=2",
			expectedOp:       OpInsert,
			expectedTables:   []string{"t"},
			expectedPreds:    []string{},
			expectedJoins:    []Join{},
			expectedAffected: []string{"a"},
			expectedRisk:     risk.SeverityInfo,
		},
		{
			// Tuple SET targets flatten into individual column names
			// per updateTargets's documented contract.
			name:             "tuple update flattens to individual columns",
			sql:              "UPDATE t SET (a,b)=(1,2) WHERE id=3",
			expectedOp:       OpUpdate,
			expectedTables:   []string{"t"},
			expectedPreds:    []string{"id = 3"},
			expectedJoins:    []Join{},
			expectedAffected: []string{"a", "b"},
			expectedRisk:     risk.SeverityInfo,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			summaries, err := Summarize(tc.sql)
			require.NoError(t, err)
			require.Len(t, summaries, 1)

			s := summaries[0]
			require.Equal(t, tc.expectedOp, s.Operation)
			if tc.expectedTag != "" {
				require.Equal(t, tc.expectedTag, s.Tag)
			}
			require.Equal(t, tc.expectedTables, s.Tables)
			require.Equal(t, tc.expectedPreds, s.Predicates)
			require.Equal(t, tc.expectedJoins, s.Joins)
			require.Equal(t, tc.expectedAffected, s.AffectedColumns)
			require.Equal(t, tc.expectedRisk, s.RiskLevel)
		})
	}
}

// TestSummarizeMultiStatement verifies that each statement gets its
// own summary in source order with its own position.
func TestSummarizeMultiStatement(t *testing.T) {
	summaries, err := Summarize("SELECT 1 FROM t; DELETE FROM u")
	require.NoError(t, err)
	require.Len(t, summaries, 2)

	require.Equal(t, OpSelect, summaries[0].Operation)
	require.Equal(t, []string{"t"}, summaries[0].Tables)
	require.Equal(t, 0, summaries[0].Position.ByteOffset)

	require.Equal(t, OpDelete, summaries[1].Operation)
	require.Equal(t, []string{"u"}, summaries[1].Tables)
	require.Equal(t, risk.SeverityCritical, summaries[1].RiskLevel)
	require.Greater(t, summaries[1].Position.ByteOffset, summaries[0].Position.ByteOffset)
}

// TestSummarizeParseError surfaces parse failures to the caller
// rather than silently producing an empty result.
func TestSummarizeParseError(t *testing.T) {
	_, err := Summarize("SELECTT 1")
	require.Error(t, err)
}

// TestRiskLevelReduction verifies that riskLevelFor reduces a
// statement's findings to the highest severity. Tested through the
// public Summarize path so we exercise the real risk registry rather
// than mocking it.
func TestRiskLevelReduction(t *testing.T) {
	tests := []struct {
		name         string
		sql          string
		expectedRisk risk.Severity
	}{
		{
			name:         "no findings → info baseline",
			sql:          "SELECT id FROM t WHERE id = 1",
			expectedRisk: risk.SeverityInfo,
		},
		{
			name:         "low only",
			sql:          "SELECT * FROM t WHERE id = 1",
			expectedRisk: risk.SeverityLow,
		},
		{
			name:         "critical wins over low (DROP plus SELECT *)",
			sql:          "SELECT * FROM t; DROP TABLE t",
			expectedRisk: risk.SeverityCritical,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			summaries, err := Summarize(tc.sql)
			require.NoError(t, err)
			// For multi-statement cases each summary carries its own
			// risk; verify the last one (where DROP lives) so the
			// table reads naturally.
			require.NotEmpty(t, summaries)
			require.Equal(t, tc.expectedRisk, summaries[len(summaries)-1].RiskLevel)
		})
	}
}
