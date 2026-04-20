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
