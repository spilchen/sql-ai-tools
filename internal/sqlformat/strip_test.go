// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sqlformat

import (
	"strings"
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

// messySQL is the verbatim transcript paste from issue #161. It
// exercises a primary prompt followed by eight padded continuation
// prompts spanning a multi-table SELECT — every line of which would
// otherwise carry stray REPL chrome past column 30.
const messySQL = "root@localhost:26257/movr> SELECT r.city,r.id AS ride_id,\n" +
	"                        -> u.name AS rider_name,v.type AS vehicle_type,\n" +
	"                        ->    r.start_address,r.end_address,\n" +
	"                        -> r.revenue FROM rides r INNER JOIN\n" +
	"                        -> users u ON r.rider_id=u.id AND r.city=u.city INNER JOIN vehicles v\n" +
	"                        ->       ON r.vehicle_id=v.id AND\n" +
	"                        -> r.vehicle_city=v.city WHERE r.city='new york'\n" +
	"                        ->  AND r.revenue > 50.00\n" +
	"                        -> ORDER BY r.revenue DESC;\n"

// TestStripShellPromptsMessyFixture pins behavior on the issue #161
// fixture: every prompt line gets cleaned, so the result parses as one
// well-formed SELECT.
func TestStripShellPromptsMessyFixture(t *testing.T) {
	want := "SELECT r.city,r.id AS ride_id,\n" +
		"u.name AS rider_name,v.type AS vehicle_type,\n" +
		"   r.start_address,r.end_address,\n" +
		"r.revenue FROM rides r INNER JOIN\n" +
		"users u ON r.rider_id=u.id AND r.city=u.city INNER JOIN vehicles v\n" +
		"      ON r.vehicle_id=v.id AND\n" +
		"r.vehicle_city=v.city WHERE r.city='new york'\n" +
		" AND r.revenue > 50.00\n" +
		"ORDER BY r.revenue DESC;\n"

	require.Equal(t, want, StripShellPrompts(messySQL))
}

// TestStripShellPromptsWithMapNoOp verifies that input without prompts
// returns Removed=false, no segments, and an identity Translate. This
// is the hot path for non-paste callers and needs to be cheap.
func TestStripShellPromptsWithMapNoOp(t *testing.T) {
	in := "SELECT 1;\nSELECT 2;\n"
	got := StripShellPromptsWithMap(in)

	require.Equal(t, in, got.Stripped)
	require.False(t, got.Removed)
	require.Empty(t, got.segments)

	// Identity for in-range offsets; negative clamps to 0; past-end
	// clamps to len(Stripped) (which equals len(original) here).
	cases := []struct {
		in, want int
	}{
		{-5, 0},
		{0, 0},
		{1, 1},
		{5, 5},
		{len(in), len(in)},
		{len(in) + 100, len(in)},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, got.Translate(tc.in), "off=%d", tc.in)
	}
}

// TestStripShellPromptsWithMapTranslatesMessyFixture asserts that byte
// offsets in the stripped messy.sql round-trip back to the right
// coordinates in the original paste. We pick semantically meaningful
// landmarks (start of statement, start of each continuation line, end
// of buffer) so a regression in the segment math fails loudly.
func TestStripShellPromptsWithMapTranslatesMessyFixture(t *testing.T) {
	got := StripShellPromptsWithMap(messySQL)
	require.True(t, got.Removed)

	// For each landmark, locate its position in the stripped buffer
	// and the original buffer separately, then assert that translating
	// the stripped offset reaches the original offset.
	cases := []string{
		"SELECT r.city",
		"u.name AS rider_name",
		"r.revenue FROM rides",
		"ORDER BY r.revenue DESC;",
	}
	for _, needle := range cases {
		strippedIdx := strings.Index(got.Stripped, needle)
		require.NotEqual(t, -1, strippedIdx, "needle %q missing from stripped", needle)
		originalIdx := strings.Index(messySQL, needle)
		require.NotEqual(t, -1, originalIdx, "needle %q missing from messy", needle)

		require.Equal(t, originalIdx, got.Translate(strippedIdx),
			"translate(%d) for %q", strippedIdx, needle)
	}

	// Edge cases: offset 0 in the stripped buffer is the 'S' of
	// "SELECT", which sat at len("root@localhost:26257/movr> ") in
	// the original. Offsets past the end clamp to len(original).
	// Negative offsets clamp to 0 then translate as 0.
	primaryPrompt := "root@localhost:26257/movr> "
	require.Equal(t, len(primaryPrompt), got.Translate(0))
	require.Equal(t, len(messySQL), got.Translate(len(got.Stripped)+100))
	require.Equal(t, len(primaryPrompt), got.Translate(-1))
}

// TestStripShellPromptsWithMapTranslateAfterPromptOnFirstLine pins the
// subtle case where the very first line carries a primary prompt: the
// stripped offset 0 still maps to original offset 0 because the prompt
// is not yet "before" any retained byte; offset 1 in stripped maps to
// original offset 1 + len("root@localhost:26257/movr> ").
func TestStripShellPromptsWithMapTranslateAfterPromptOnFirstLine(t *testing.T) {
	in := "root@localhost:26257/movr> SELECT 1;\n"
	got := StripShellPromptsWithMap(in)
	require.True(t, got.Removed)
	require.Equal(t, "SELECT 1;\n", got.Stripped)

	// "S" sits at stripped offset 0; in the original it sits at the
	// length of the primary prompt (including its trailing space).
	prompt := "root@localhost:26257/movr> "
	require.Equal(t, len(prompt), got.Translate(0))
	require.Equal(t, len(prompt)+5, got.Translate(5))
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
