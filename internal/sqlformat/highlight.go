// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sqlformat

import (
	"strings"
	"unicode"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/lexbase"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/scanner"
)

// ANSI SGR escape sequences used by Highlight. Bundled here so a future
// theming option only needs to swap this table; callers never construct
// escape sequences directly.
const (
	ansiReset     = "\x1b[0m"
	ansiKeyword   = "\x1b[1;36m" // bold cyan
	ansiStringLit = "\x1b[32m"   // green
	ansiNumberLit = "\x1b[35m"   // magenta
)

// firstKeywordTokenID is the lowest token ID assigned to a SQL keyword
// in lexbase/tokens.go. Token IDs below it are reserved for the lexer's
// non-keyword categories (IDENT, SCONST, BCONST, BITCONST, ICONST,
// FCONST, PLACEHOLDER, the multi-char operators, and ERROR). Above this
// boundary, the alphabetical block of ~1500 keywords (ABORT, ABSOLUTE,
// …) accounts for nearly all token IDs; a small tail of synthetic
// tokens (HELPTOKEN, POSTFIXOP, UMINUS, the *_LA lookahead pseudo-
// tokens) sits above the keyword block but never appears in
// formatter-produced output as all-uppercase letter text, so the
// isAllUpperLetters check in colorFor disambiguates without us having
// to enumerate them. The boundary value is taken from lexbase.ABORT,
// the alphabetically-first keyword.
const firstKeywordTokenID = lexbase.ABORT

// Highlight returns formatted with ANSI SGR escape sequences wrapping
// SQL keywords (bold cyan), string literals (green), and numeric
// literals (magenta). Identifiers, punctuation, and whitespace are
// emitted uncolored. The function preserves byte ranges between tokens
// verbatim, so the visible (de-escaped) output is byte-identical to
// formatted.
//
// Tokenization is performed with scanner.Inspect, which yields token
// start/end offsets into formatted. If any token comes back as ERROR
// or as the incomplete-token sentinel (negative ID, documented in
// scanner.Inspect), Highlight returns formatted unchanged rather than
// emit a half-colored garbage string. In normal use this fallback
// never fires because callers feed already-validated SQL, but the
// defensive check protects against bounds-violating slices if a future
// caller passes hand-constructed input.
//
// Highlight is intended for terminal output only. JSON envelopes must
// never contain escape sequences, so callers gate this behind a
// text-mode + TTY check.
func Highlight(formatted string) string {
	if formatted == "" {
		return formatted
	}

	tokens := scanner.Inspect(formatted)
	for _, tok := range tokens {
		// ERROR signals a scan failure; ID < 0 is the incomplete-token
		// sentinel scanner.Inspect appends when input ends mid-token.
		// Either case means downstream slicing on tok.Start/tok.End
		// would be unsound, so fall back to the uncolored input.
		if tok.ID == lexbase.ERROR || tok.ID < 0 {
			return formatted
		}
	}

	var (
		buf     strings.Builder
		lastEnd int32
	)
	buf.Grow(len(formatted) + len(tokens)*8)

	for _, tok := range tokens {
		// scanner.Inspect appends a sentinel token with ID 0 at EOF.
		// It carries no text and must be skipped before color logic
		// runs, otherwise the trailing-text copy below would be
		// duplicated.
		if tok.ID == 0 {
			break
		}

		if tok.Start > lastEnd {
			buf.WriteString(formatted[lastEnd:tok.Start])
		}

		text := formatted[tok.Start:tok.End]
		if color := colorFor(tok.ID, text); color != "" {
			buf.WriteString(color)
			buf.WriteString(text)
			buf.WriteString(ansiReset)
		} else {
			buf.WriteString(text)
		}
		lastEnd = tok.End
	}

	if int(lastEnd) < len(formatted) {
		buf.WriteString(formatted[lastEnd:])
	}
	return buf.String()
}

// colorFor returns the ANSI SGR prefix for a token, or the empty
// string if the token should be emitted uncolored. Empty-string return
// signals the caller to skip the wrap-with-reset path; it is not a
// default color.
//
// Keyword classification is deliberately conservative. CockroachDB's
// lexer assigns keyword IDs to many words that are also valid
// identifiers in PostgreSQL grammar (USERS, NAME, USER, COMMENT, KEY,
// VALUE, …). Coloring every keyword-ID token would highlight a table
// named "users" or a column named "name" as if it were a SQL keyword,
// which is visually noisy and misleading. We sidestep this by
// piggy-backing on the pretty-printer's own decision: keywords used
// grammatically as keywords are emitted UPPERCASE, identifiers are
// emitted in their original case (or quoted). So a keyword-ID token is
// colored only when its text is all-uppercase letters in the formatted
// output. This is a heuristic, not a parse-aware classification, but
// it matches the formatter's contract and avoids identifier mis-color.
func colorFor(id int32, text string) string {
	switch id {
	case lexbase.SCONST, lexbase.BCONST, lexbase.BITCONST:
		return ansiStringLit
	case lexbase.ICONST, lexbase.FCONST:
		return ansiNumberLit
	}
	if id >= firstKeywordTokenID && isAllUpperLetters(text) {
		return ansiKeyword
	}
	return ""
}

// isAllUpperLetters reports whether s is non-empty and every letter
// rune in s is uppercase. Non-letter runes (digits, underscores) are
// ignored so that words like "INT8" still classify as keyword-shaped.
func isAllUpperLetters(s string) bool {
	if s == "" {
		return false
	}
	hasLetter := false
	for _, r := range s {
		if !unicode.IsLetter(r) {
			continue
		}
		hasLetter = true
		if !unicode.IsUpper(r) {
			return false
		}
	}
	return hasLetter
}
