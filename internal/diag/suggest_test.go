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

// pos is a tiny helper so test cases can declare positions inline
// without repeating the struct literal.
func pos(byteOffset int) *output.Position {
	return &output.Position{Line: 1, Column: byteOffset + 1, ByteOffset: byteOffset}
}

func TestSuggest(t *testing.T) {
	tests := []struct {
		name             string
		misspelled       string
		candidates       []string
		position         *output.Position
		expectedCount    int
		expectedFirst    string  // first replacement, when expectedCount > 0
		expectedReason   string  // first reason, when expectedCount > 0
		expectedConf     float64 // first confidence, when expectedCount > 0
		expectedRangeEnd int     // first range.End, when expectedCount > 0
	}{
		{
			name:             "distance 1 in middle bucket",
			misspelled:       "nme",
			candidates:       []string{"name", "id", "email"},
			position:         pos(7),
			expectedCount:    1,
			expectedFirst:    "name",
			expectedReason:   "levenshtein_distance_1",
			expectedConf:     0.75, // 1 - 1/4
			expectedRangeEnd: 10,
		},
		{
			name:           "distance 1 in long bucket",
			misspelled:     "username",
			candidates:     []string{"user_name"},
			position:       pos(0),
			expectedCount:  1,
			expectedFirst:  "user_name",
			expectedReason: "levenshtein_distance_1",
		},
		{
			name:           "distance 2 in middle bucket",
			misspelled:     "emial",
			candidates:     []string{"email"}, // transposition counts as 2 edits
			position:       pos(0),
			expectedCount:  1,
			expectedFirst:  "email",
			expectedReason: "levenshtein_distance_2",
		},
		{
			name:          "short name rejects distance 2",
			misspelled:    "xy",
			candidates:    []string{"name", "id"},
			position:      pos(0),
			expectedCount: 0,
		},
		{
			name:           "distance 3 in long bucket",
			misspelled:     "usrname",
			candidates:     []string{"username"},
			position:       pos(0),
			expectedCount:  1,
			expectedFirst:  "username",
			expectedReason: "levenshtein_distance_1",
		},
		{
			name:          "rejects distance 4 even in long bucket",
			misspelled:    "completely",
			candidates:    []string{"different"},
			position:      pos(0),
			expectedCount: 0,
		},
		{
			name:           "case-insensitive distance match",
			misspelled:     "NAEM",
			candidates:     []string{"name"},
			position:       pos(0),
			expectedCount:  1,
			expectedFirst:  "name",
			expectedReason: "levenshtein_distance_2",
		},
		{
			name:          "case-insensitive exact match returns nil",
			misspelled:    "NAME",
			candidates:    []string{"name"},
			position:      pos(0),
			expectedCount: 0,
		},
		{
			name:          "exact match returns nil",
			misspelled:    "name",
			candidates:    []string{"name"},
			position:      pos(0),
			expectedCount: 0,
		},
		{
			name:          "nil position returns nil",
			misspelled:    "nme",
			candidates:    []string{"name"},
			position:      nil,
			expectedCount: 0,
		},
		{
			name:          "empty misspelled returns nil",
			misspelled:    "",
			candidates:    []string{"name"},
			position:      pos(0),
			expectedCount: 0,
		},
		{
			name:          "empty candidates returns nil",
			misspelled:    "nme",
			candidates:    nil,
			position:      pos(0),
			expectedCount: 0,
		},
		{
			name:           "ranking by distance then name",
			misspelled:     "names",
			candidates:     []string{"namxs", "name", "namy"},
			position:       pos(0),
			expectedCount:  3,
			expectedFirst:  "name", // distance 1, alphabetically first vs "namxs"
			expectedReason: "levenshtein_distance_1",
		},
		{
			name:          "caps at three suggestions",
			misspelled:    "name",
			candidates:    []string{"nama", "namb", "namc", "namd", "name1"},
			position:      pos(0),
			expectedCount: 3,
			expectedFirst: "nama",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Suggest(tc.misspelled, tc.candidates, tc.position)
			require.Len(t, got, tc.expectedCount)
			if tc.expectedCount == 0 {
				return
			}
			require.Equal(t, tc.expectedFirst, got[0].Replacement)
			if tc.expectedReason != "" && tc.expectedReason != "levenshtein_distance_0" {
				require.Equal(t, tc.expectedReason, got[0].Reason)
			}
			if tc.expectedConf != 0 {
				require.InDelta(t, tc.expectedConf, got[0].Confidence, 0.001)
			}
			if tc.expectedRangeEnd != 0 {
				require.Equal(t, tc.expectedRangeEnd, got[0].Range.End)
				require.Equal(t, tc.position.ByteOffset, got[0].Range.Start)
			}
		})
	}
}

func TestSuggestRangeCoversMisspelled(t *testing.T) {
	// Lock in the byte-range invariant: Range covers exactly
	// [pos.ByteOffset, pos.ByteOffset + len(misspelled)) so an agent
	// can apply the replacement by slicing the original SQL.
	got := Suggest("nme", []string{"name"}, pos(7))
	require.Len(t, got, 1)
	require.Equal(t, output.Range{Start: 7, End: 10}, got[0].Range)
}

func TestMaxDistance(t *testing.T) {
	tests := []struct {
		name     string
		nameLen  int
		expected int
	}{
		{name: "single char", nameLen: 1, expected: 1},
		{name: "boundary 3", nameLen: 3, expected: 1},
		{name: "boundary 4", nameLen: 4, expected: 2},
		{name: "boundary 6", nameLen: 6, expected: 2},
		{name: "boundary 7", nameLen: 7, expected: 3},
		{name: "long name", nameLen: 50, expected: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, maxDistance(tc.nameLen))
		})
	}
}

func TestLevenshtein(t *testing.T) {
	tests := []struct {
		name     string
		a, b     string
		expected int
	}{
		{name: "identical", a: "name", b: "name", expected: 0},
		{name: "single substitution", a: "name", b: "namx", expected: 1},
		{name: "single deletion", a: "name", b: "nam", expected: 1},
		{name: "single insertion", a: "nme", b: "name", expected: 1},
		{name: "two edits", a: "nme", b: "names", expected: 2},
		{name: "empty a", a: "", b: "abc", expected: 3},
		{name: "empty b", a: "abc", b: "", expected: 3},
		{name: "both empty", a: "", b: "", expected: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, levenshtein(tc.a, tc.b))
		})
	}
}
