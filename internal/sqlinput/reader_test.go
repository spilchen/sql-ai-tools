// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sqlinput

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadSQL(t *testing.T) {
	// Write a temp file for the file-arg test cases.
	dir := t.TempDir()
	sqlFile := filepath.Join(dir, "input.sql")
	require.NoError(t, os.WriteFile(sqlFile, []byte("SELECT 42"), 0644))

	emptyFile := filepath.Join(dir, "empty.sql")
	require.NoError(t, os.WriteFile(emptyFile, []byte("   \n"), 0644))

	tests := []struct {
		name        string
		expr        string
		args        []string
		stdin       string
		expected    string
		expectedErr string
	}{
		{
			name:     "inline via -e flag",
			expr:     "SELECT 1",
			expected: "SELECT 1",
		},
		{
			name:     "file argument",
			args:     []string{sqlFile},
			expected: "SELECT 42",
		},
		{
			name:     "stdin",
			stdin:    "SELECT 99",
			expected: "SELECT 99",
		},
		{
			name:     "-e takes priority over stdin",
			expr:     "SELECT 1",
			stdin:    "SELECT 2",
			expected: "SELECT 1",
		},
		{
			name:        "conflict -e and file arg",
			expr:        "SELECT 1",
			args:        []string{sqlFile},
			expectedErr: "cannot use -e flag and file argument together",
		},
		{
			name:        "file not found",
			args:        []string{filepath.Join(dir, "nonexistent.sql")},
			expectedErr: "read SQL file",
		},
		{
			name:        "empty file rejected",
			args:        []string{emptyFile},
			expectedErr: "is empty",
		},
		{
			name:        "empty stdin rejected",
			stdin:       "",
			expectedErr: "no SQL input provided",
		},
		{
			name:        "whitespace-only stdin rejected",
			stdin:       "   \n\t  ",
			expectedErr: "no SQL input provided",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdin *strings.Reader
			if tc.stdin != "" || (tc.expr == "" && len(tc.args) == 0) {
				stdin = strings.NewReader(tc.stdin)
			}

			got, err := ReadSQL(tc.expr, tc.args, stdin)
			if tc.expectedErr != "" {
				require.ErrorContains(t, err, tc.expectedErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expected, got)
		})
	}
}
