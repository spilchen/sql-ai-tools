// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package diag

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/output"
)

func TestSuggestKeyword(t *testing.T) {
	pos := &output.Position{Line: 1, Column: 1, ByteOffset: 0}

	tests := []struct {
		name             string
		token            string
		pos              *output.Position
		expectedFirst    string
		expectedReason   string
		expectedRangeEnd int
		expectNoneAtAll  bool
	}{
		{
			name:             "transposition typo (Damerau-1, classic Lev-2)",
			token:            "FORM",
			pos:              pos,
			expectedFirst:    "from",
			expectedReason:   "damerau_levenshtein_distance_1",
			expectedRangeEnd: 4,
		},
		{
			name:             "deletion typo at end",
			token:            "SELECTT",
			pos:              pos,
			expectedFirst:    "select",
			expectedReason:   "damerau_levenshtein_distance_1",
			expectedRangeEnd: 7,
		},
		{
			name:             "deletion typo in middle",
			token:            "WHRE",
			pos:              pos,
			expectedFirst:    "where",
			expectedReason:   "damerau_levenshtein_distance_1",
			expectedRangeEnd: 4,
		},
		{
			name:            "distance-3 rejected (cap is 2)",
			token:           "ZZZZZZZZ",
			pos:             pos,
			expectNoneAtAll: true,
		},
		{
			name:            "exact-match keyword skipped",
			token:           "SELECT",
			pos:             pos,
			expectNoneAtAll: true,
		},
		{
			name:            "empty token returns nil",
			token:           "",
			pos:             pos,
			expectNoneAtAll: true,
		},
		{
			name:            "nil pos returns nil",
			token:           "FORM",
			pos:             nil,
			expectNoneAtAll: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SuggestKeyword(tc.token, tc.pos)
			if tc.expectNoneAtAll {
				require.Nil(t, got)
				return
			}
			require.NotEmpty(t, got)
			require.LessOrEqual(t, len(got), 3)
			require.Equal(t, tc.expectedFirst, got[0].Replacement)
			require.Equal(t, tc.expectedReason, got[0].Reason)
			require.Equal(t, tc.pos.ByteOffset, got[0].Range.Start)
			require.Equal(t, tc.expectedRangeEnd, got[0].Range.End)
			require.Greater(t, got[0].Confidence, 0.0)
			require.LessOrEqual(t, got[0].Confidence, 1.0)
		})
	}
}

func TestIsKeyword(t *testing.T) {
	tests := []struct {
		name     string
		token    string
		expected bool
	}{
		{name: "lower-case keyword", token: "select", expected: true},
		{name: "upper-case keyword", token: "SELECT", expected: true},
		{name: "mixed-case keyword", token: "SeLeCt", expected: true},
		{name: "non-keyword identifier", token: "rideshare_trips", expected: false},
		{name: "empty string", token: "", expected: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, IsKeyword(tc.token))
		})
	}
}

// TestDamerauLevenshtein pins the metric independently of
// SuggestKeyword. The DP differs from classic Levenshtein only on
// adjacent transpositions (the OSA cell), so cases that reduce to
// pure insertion / deletion / substitution should match levenshtein
// outputs exactly. The transposition cases are the headline
// behavior; without them a regression that silently downgrades to
// classic Levenshtein would leave SuggestKeyword shipping the wrong
// distance metadata for FORM/FROM-style typos.
func TestDamerauLevenshtein(t *testing.T) {
	tests := []struct {
		name     string
		a, b     string
		expected int
	}{
		{name: "identical", a: "form", b: "form", expected: 0},
		{name: "empty a", a: "", b: "abc", expected: 3},
		{name: "empty b", a: "abc", b: "", expected: 3},
		{name: "both empty", a: "", b: "", expected: 0},
		{name: "single substitution", a: "form", b: "fort", expected: 1},
		{name: "single deletion", a: "form", b: "for", expected: 1},
		{name: "single insertion", a: "for", b: "form", expected: 1},
		{name: "minimum transposition (ab/ba)", a: "ab", b: "ba", expected: 1},
		{name: "form/from is one transposition", a: "form", b: "from", expected: 1},
		{name: "tbale/table is one transposition", a: "tbale", b: "table", expected: 1},
		{name: "transposition not at edge (acbd)", a: "abcd", b: "acbd", expected: 1},
		{
			// OSA upper bound: each substring may participate in at
			// most one transposition, so "ca" → "abc" cannot reuse
			// the c↔a swap to cover the trailing insertion. True
			// Damerau distance would be 2; OSA gives 3. Locks in the
			// "OSA, not full Damerau" contract called out in the
			// docstring.
			name: "OSA contract: ca/abc is 3, not 2", a: "ca", b: "abc", expected: 3,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, damerauLevenshtein(tc.a, tc.b))
		})
	}
}

func TestKeywordCandidatesCached(t *testing.T) {
	a := keywordCandidates()
	b := keywordCandidates()
	require.NotEmpty(t, a)
	require.True(t, len(a) > 100, "keyword candidate list should pull in the full lexbase set")
	// require.Same checks pointer identity, which actually proves the
	// sync.Once memoization (require.Equal on *string would also pass
	// for two distinct slices that happen to share their first
	// element).
	require.Same(t, &a[0], &b[0])
}
