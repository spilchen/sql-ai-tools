// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sqlformat

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStripShellPrompts(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty input",
			input:    "",
			expected: "",
		},
		{
			name:     "no prompt is a no-op",
			input:    "SELECT 1;\nSELECT 2;\n",
			expected: "SELECT 1;\nSELECT 2;\n",
		},
		{
			name:     "single primary prompt with root user",
			input:    "root@:26257/defaultdb> SELECT 1;\n",
			expected: "SELECT 1;\n",
		},
		{
			name:     "primary prompt with non-root user",
			input:    "app@host:26257/defaultdb> SELECT 1;\n",
			expected: "SELECT 1;\n",
		},
		{
			name:     "primary prompt with dotted username",
			input:    "marc.smith@host:26257/db> SELECT 1;\n",
			expected: "SELECT 1;\n",
		},
		{
			name:     "primary prompt with underscore username",
			input:    "app_user@host/db> SELECT 1;\n",
			expected: "SELECT 1;\n",
		},
		{
			name:     "primary prompt with OPEN state word",
			input:    "root@:26257/defaultdb OPEN> SELECT 1;\n",
			expected: "SELECT 1;\n",
		},
		{
			name:     "primary prompt with ERROR state word",
			input:    "root@:26257/defaultdb ERROR> SELECT 1;\n",
			expected: "SELECT 1;\n",
		},
		{
			name: "padded continuation prompts after primary",
			input: "root@:26257/defaultdb> SELECT id,\n" +
				"                    ->   name,\n" +
				"                    ->   email\n" +
				"                    -> FROM users;\n",
			expected: "SELECT id,\n  name,\n  email\nFROM users;\n",
		},
		{
			name:     "continuation without preceding primary is left alone",
			input:    "SELECT data\n  -> 'key'\nFROM t;\n",
			expected: "SELECT data\n  -> 'key'\nFROM t;\n",
		},
		{
			name: "multi-statement transcript",
			input: "root@:26257/defaultdb> SELECT 1;\n" +
				"root@:26257/defaultdb> SELECT 2;\n",
			expected: "SELECT 1;\nSELECT 2;\n",
		},
		{
			name:     "primary prompt mid-line is not stripped",
			input:    "-- see root@host:26257/db> for details\n",
			expected: "-- see root@host:26257/db> for details\n",
		},
		{
			name:     "blank lines are preserved",
			input:    "root@:26257/defaultdb> SELECT 1;\n\nroot@:26257/defaultdb> SELECT 2;\n",
			expected: "SELECT 1;\n\nSELECT 2;\n",
		},
		{
			name:     "trailing prompt with no command",
			input:    "root@:26257/defaultdb>\n",
			expected: "\n",
		},
		{
			name:     "no trailing newline",
			input:    "root@:26257/defaultdb> SELECT 1",
			expected: "SELECT 1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := StripShellPrompts(tc.input)
			require.Equal(t, tc.expected, got)
		})
	}
}

// TestStripShellPromptsRoundTripsThroughFormat verifies that a pasted
// REPL transcript, after stripping, parses and pretty-prints cleanly.
// This is the end-to-end contract callers (cmd/format, MCP format_sql)
// rely on.
func TestStripShellPromptsRoundTripsThroughFormat(t *testing.T) {
	pasted := "root@:26257/defaultdb> SELECT id,\n" +
		"                    ->   name\n" +
		"                    -> FROM users;\n"

	formatted, err := Format(StripShellPrompts(pasted))
	require.NoError(t, err)
	require.Contains(t, formatted, "SELECT id, name FROM users")
}
