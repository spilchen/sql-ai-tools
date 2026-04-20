// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sqlformat

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFormat(t *testing.T) {
	tests := []struct {
		name           string
		input          string
		expectedOutput string
		expectedErr    string
	}{
		{
			name:           "passes through clean SQL",
			input:          "SELECT 1",
			expectedOutput: "SELECT 1",
		},
		{
			name:           "canonicalizes whitespace and casing",
			input:          "select  id,name  from  users",
			expectedOutput: "SELECT id, name FROM users",
		},
		{
			name:           "multi-statement separated by semicolon-newline",
			input:          "SELECT 1; SELECT 2",
			expectedOutput: "SELECT 1;\nSELECT 2",
		},
		{
			name:           "complex DDL wraps lines",
			input:          "CREATE TABLE t (a INT PRIMARY KEY, b TEXT NOT NULL, c FLOAT)",
			expectedOutput: "CREATE TABLE t (\n\ta INT8 PRIMARY KEY, b STRING NOT NULL, c FLOAT8\n)",
		},
		{
			name:           "empty input",
			input:          "",
			expectedOutput: "",
		},
		{
			name:        "parse error",
			input:       "SELECTT 1",
			expectedErr: "syntax error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Format(tc.input)
			if tc.expectedErr != "" {
				require.ErrorContains(t, err, tc.expectedErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expectedOutput, got)
		})
	}
}
