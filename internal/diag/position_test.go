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

func TestExtractPosition(t *testing.T) {
	tests := []struct {
		name     string
		detail   string
		fullSQL  string
		expected *output.Position
	}{
		{
			name:    "error at EOF of single-line statement",
			detail:  "source SQL:\nSELECT FROM\n           ^",
			fullSQL: "SELECT FROM",
			expected: &output.Position{
				Line:       1,
				Column:     12,
				ByteOffset: 11,
			},
		},
		{
			name:    "error at start of statement",
			detail:  "source SQL:\nSELECTT 1\n^",
			fullSQL: "SELECTT 1",
			expected: &output.Position{
				Line:       1,
				Column:     1,
				ByteOffset: 0,
			},
		},
		{
			name:    "error mid-token",
			detail:  "source SQL:\nCREATE TABL foo (id INT)\n       ^",
			fullSQL: "CREATE TABL foo (id INT)",
			expected: &output.Position{
				Line:       1,
				Column:     8,
				ByteOffset: 7,
			},
		},
		{
			name:    "multi-statement error on second statement",
			detail:  "source SQL:\nSELECT FROM\n           ^",
			fullSQL: "SELECT 1;\nSELECT FROM",
			expected: &output.Position{
				Line:       2,
				Column:     12,
				ByteOffset: 21,
			},
		},
		{
			name:    "multi-statement error on second statement same line",
			detail:  "source SQL:\nSELECT FROM\n           ^",
			fullSQL: "SELECT 1; SELECT FROM",
			expected: &output.Position{
				Line:       1,
				Column:     22,
				ByteOffset: 21,
			},
		},
		{
			name:    "multi-line statement fragment",
			detail:  "source SQL:\nSELECT\n  1\n  FROM\n      ^",
			fullSQL: "SELECT\n  1\n  FROM",
			expected: &output.Position{
				Line:       3,
				Column:     7,
				ByteOffset: 17,
			},
		},
		{
			name:    "fragment appears in earlier valid statement",
			detail:  "source SQL:\nSELECT 1 FROM\n             ^",
			fullSQL: "SELECT 1 FROM t; SELECT 1 FROM",
			expected: &output.Position{
				Line:       1,
				Column:     31,
				ByteOffset: 30,
			},
		},
		{
			name:     "fragment not found in full SQL",
			detail:   "source SQL:\nNOT HERE\n        ^",
			fullSQL:  "SELECT 1",
			expected: nil,
		},
		{
			name:     "empty detail",
			detail:   "",
			fullSQL:  "SELECT 1",
			expected: nil,
		},
		{
			name:     "malformed detail without prefix",
			detail:   "some other error text",
			fullSQL:  "SELECT 1",
			expected: nil,
		},
		{
			name:     "detail missing caret",
			detail:   "source SQL:\nSELECT FROM\nno caret here",
			fullSQL:  "SELECT FROM",
			expected: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractPosition(tc.detail, tc.fullSQL)
			if tc.expected == nil {
				require.Nil(t, got)
			} else {
				require.NotNil(t, got)
				require.Equal(t, *tc.expected, *got)
			}
		})
	}
}

func TestCharIndexToByteOffset(t *testing.T) {
	tests := []struct {
		name           string
		sql            string
		charIdx        int
		expectedOffset int
	}{
		{name: "negative clamps to zero", sql: "abc", charIdx: -5, expectedOffset: 0},
		{name: "zero is start of string", sql: "abc", charIdx: 0, expectedOffset: 0},
		{name: "ascii midstring is identity", sql: "SELECT 1", charIdx: 7, expectedOffset: 7},
		{name: "multibyte rune adds bytes", sql: `"é"x`, charIdx: 3, expectedOffset: 4},
		{name: "exact end of string", sql: "abc", charIdx: 3, expectedOffset: 3},
		{name: "past end clamps to len", sql: "abc", charIdx: 99, expectedOffset: 3},
		{name: "empty string clamps to zero", sql: "", charIdx: 5, expectedOffset: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CharIndexToByteOffset(tc.sql, tc.charIdx)
			require.Equal(t, tc.expectedOffset, got)
		})
	}
}

func TestLineColumn(t *testing.T) {
	tests := []struct {
		name         string
		sql          string
		byteOffset   int
		expectedLine int
		expectedCol  int
	}{
		{
			name:         "start of single-line input",
			sql:          "SELECT 1",
			byteOffset:   0,
			expectedLine: 1,
			expectedCol:  1,
		},
		{
			name:         "middle of single-line input",
			sql:          "SELECT 1",
			byteOffset:   7,
			expectedLine: 1,
			expectedCol:  8,
		},
		{
			name:         "start of second line",
			sql:          "SELECT 1;\nSELECT 2",
			byteOffset:   10,
			expectedLine: 2,
			expectedCol:  1,
		},
		{
			name:         "middle of second line",
			sql:          "SELECT 1;\nSELECT 2",
			byteOffset:   17,
			expectedLine: 2,
			expectedCol:  8,
		},
		{
			name:         "offset beyond input clamped",
			sql:          "SELECT 1",
			byteOffset:   100,
			expectedLine: 1,
			expectedCol:  9,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			line, col := lineColumn(tc.sql, tc.byteOffset)
			require.Equal(t, tc.expectedLine, line, "line")
			require.Equal(t, tc.expectedCol, col, "column")
		})
	}
}

// TestAdjustPosition pins the contract that AdjustPosition translates
// stripped-buffer Positions back to original-input coordinates and
// re-derives Line/Column against the original. The translate function
// in each case is the simplest possible non-trivial mapper: a constant
// prefix offset added to every input. This isolates the AdjustPosition
// logic from the more elaborate sqlformat.StripResult.Translate logic
// covered separately in internal/sqlformat/strip_test.go.
func TestAdjustPosition(t *testing.T) {
	const original = "root@:26257/db> SELECT 1;\nSELECT bad FROM" // 41 bytes total
	const promptLen = len("root@:26257/db> ")                     // 16

	identity := func(off int) int { return off }
	addPrompt := func(off int) int { return off + promptLen }

	tests := []struct {
		name        string
		pos         *output.Position
		originalSQL string
		translate   func(int) int
		expected    *output.Position
	}{
		{
			name:      "nil position is preserved",
			pos:       nil,
			translate: identity,
			expected:  nil,
		},
		{
			name:        "identity translate re-derives line/column unchanged",
			pos:         &output.Position{Line: 99, Column: 99, ByteOffset: 0},
			originalSQL: original,
			translate:   identity,
			expected:    &output.Position{Line: 1, Column: 1, ByteOffset: 0},
		},
		{
			name:        "prompt-prefix translate shifts byte offset and column",
			pos:         &output.Position{Line: 1, Column: 1, ByteOffset: 0},
			originalSQL: original,
			translate:   addPrompt,
			expected:    &output.Position{Line: 1, Column: promptLen + 1, ByteOffset: promptLen},
		},
		{
			name:        "translate across newline updates line number",
			pos:         &output.Position{Line: 1, Column: 1, ByteOffset: 10}, // "SELECT bad" first byte in stripped
			originalSQL: original,
			// In stripped buffer offset 10 is 'S' of "SELECT bad". After
			// the prompt translate it lands at byte 26 of original, the
			// 'S' of the second statement on line 2.
			translate: addPrompt,
			expected:  &output.Position{Line: 2, Column: 1, ByteOffset: 26},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := AdjustPosition(tc.pos, tc.originalSQL, tc.translate)
			require.Equal(t, tc.expected, got)
		})
	}
}
