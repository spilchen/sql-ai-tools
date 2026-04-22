// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sqlformat

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// ansiSGR matches any ANSI SGR escape sequence so tests can strip
// coloring before comparing visible content.
var ansiSGR = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string {
	return ansiSGR.ReplaceAllString(s, "")
}

func TestHighlight_VisibleContentUnchanged(t *testing.T) {
	tests := []string{
		"SELECT 1",
		"SELECT id, name FROM users",
		"SELECT 1;\nSELECT 2",
		"INSERT INTO t VALUES (1, 'hello', 3.14)",
		"SELECT * FROM t WHERE x = 'a''b'",
		"SELECT 'café', 'naïve' FROM t",
	}
	for _, in := range tests {
		t.Run(in, func(t *testing.T) {
			require.Equal(t, in, stripANSI(Highlight(in)),
				"Highlight must preserve visible bytes after stripping ANSI escapes")
		})
	}
}

func TestHighlight_KeywordColored(t *testing.T) {
	got := Highlight("SELECT 1")
	require.Contains(t, got, ansiKeyword+"SELECT"+ansiReset)
}

func TestHighlight_StringLiteralColored(t *testing.T) {
	got := Highlight("SELECT 'hello'")
	require.Contains(t, got, ansiStringLit+"'hello'"+ansiReset)
}

func TestHighlight_NumericLiteralColored(t *testing.T) {
	tests := []struct {
		name  string
		input string
		token string
	}{
		{name: "integer", input: "SELECT 42", token: "42"},
		{name: "float", input: "SELECT 3.14", token: "3.14"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Contains(t, Highlight(tc.input), ansiNumberLit+tc.token+ansiReset)
		})
	}
}

func TestHighlight_IdentifierUncolored(t *testing.T) {
	got := Highlight("SELECT users")
	// The bare identifier "users" must not be wrapped in any color.
	require.NotContains(t, got, ansiKeyword+"users")
	require.NotContains(t, got, ansiStringLit+"users")
	require.NotContains(t, got, ansiNumberLit+"users")
	// And it must be present verbatim.
	require.Contains(t, got, "users")
}

func TestHighlight_PunctuationUncolored(t *testing.T) {
	got := Highlight("SELECT 1, 2")
	// Comma must not be wrapped in any of the open-color escapes. (It
	// will follow ansiReset from the preceding number; that's expected.)
	for _, color := range []string{ansiKeyword, ansiStringLit, ansiNumberLit} {
		require.NotContains(t, got, color+",", "comma must not be opened with %q", color)
	}
}

func TestHighlight_MultiStatementSeparatorPreserved(t *testing.T) {
	got := Highlight("SELECT 1;\nSELECT 2")
	require.Equal(t, "SELECT 1;\nSELECT 2", stripANSI(got))
	want := ansiKeyword + "SELECT" + ansiReset
	matches := regexp.MustCompile(regexp.QuoteMeta(want)).FindAllString(got, -1)
	require.Len(t, matches, 2, "both SELECT keywords must be colored")
}

func TestHighlight_TokenizationFailureFallsBack(t *testing.T) {
	// An unterminated string literal causes the scanner to emit ERROR.
	// Highlight must return the input unchanged rather than emit a
	// half-colored output.
	in := "SELECT 'unterminated"
	require.Equal(t, in, Highlight(in))
}

func TestHighlight_EmptyInput(t *testing.T) {
	require.Equal(t, "", Highlight(""))
}
