// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package diag

import (
	"sort"
	"sync"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/lexbase"

	"github.com/spilchen/sql-ai-tools/internal/output"
)

// keywordSuggestionDistanceCap bounds keyword "did you mean?" hits to
// Damerau-Levenshtein distance ≤ 2 (the metric SuggestKeyword uses).
// The full SQL keyword set is ~1300 entries, so a distance-3 match
// against a short token is almost always coincidence (e.g. "FORM" ↔
// "FROM" is distance 1 via one transposition; "FORK" ↔ "FROM" is
// distance 2 via one transposition + one substitution; "FACT" ↔
// "FROM" would be distance 3 — not a useful fix). The general
// Suggest path uses 1/2/3 by length; keywords cap tighter.
const keywordSuggestionDistanceCap = 2

var (
	keywordCandidatesOnce sync.Once
	keywordCandidatesList []string
)

// keywordCandidates returns the lower-cased, sorted list of every SQL
// keyword recognized by the cockroachdb-parser lexer. The underlying
// table (lexbase.KeywordsCategories) is a map so its keys are unique
// by construction — no dedup pass is needed. Categories present:
// R=reserved, U=unreserved, C=context-sensitive, T=type-and-function
// name (the four single-letter codes used in the map's values).
//
// The list is built once per process and cached by reference; callers
// must treat the returned slice as read-only.
//
// We deliberately wrap the parser's keyword table rather than
// hand-maintaining a list — the parser file is generated and stays in
// sync with the grammar.
func keywordCandidates() []string {
	keywordCandidatesOnce.Do(func() {
		keywordCandidatesList = make([]string, 0, len(lexbase.KeywordsCategories))
		for kw := range lexbase.KeywordsCategories {
			keywordCandidatesList = append(keywordCandidatesList, kw)
		}
		sort.Strings(keywordCandidatesList)
	})
	return keywordCandidatesList
}

// IsKeyword reports whether token (case-insensitively) is a recognized
// SQL keyword. lexbase.KeywordsCategories keys are stored lower-cased,
// so the lookup converts in place.
func IsKeyword(token string) bool {
	if token == "" {
		return false
	}
	_, ok := lexbase.KeywordsCategories[lowerASCII(token)]
	return ok
}

// SuggestKeyword returns up to three "did you mean?" fix suggestions
// when token is close to a recognized SQL keyword under
// Damerau-Levenshtein distance. The cap is min(2,
// maxDistance(len(token))), so tokens up to 3 characters are capped
// at 1 edit and longer tokens at 2 edits — see
// keywordSuggestionDistanceCap for the rationale, and maxDistance
// for the per-length scaling. Suggestions carry a Reason prefix of
// ReasonDamerauLevenshtein, so callers branching on Reason can tell
// this metric apart from the classic Levenshtein suggestions emitted
// by Suggest.
//
// SuggestKeyword is the keyword-typo equivalent of Suggest, used by
// FromParseError to enrich syntax errors.
//
// Returns nil under the same conditions as Suggest (empty token, nil
// pos, no candidate within distance). It additionally returns nil
// when token is itself a recognized SQL keyword: the error is then a
// misuse-of-keyword situation (wrong keyword for this grammatical
// slot, e.g. `INSERT FROM t` flags FROM because INSERT requires
// INTO), not a typo, so a "did you mean?" would either echo the same
// word back or fire on a coincidentally-close keyword.
//
// Token preconditions: SuggestKeyword does not validate that token
// looks like an identifier; passing "3abc" or "(*)" will run the DP
// against the full keyword candidate list and may return a
// coincidental hit. The intended caller is FromParseError, which
// uses identifierAt to filter non-identifier offsets before
// invoking; standalone callers should apply equivalent filtering.
func SuggestKeyword(token string, pos *output.Position) []output.Suggestion {
	if token == "" || pos == nil {
		return nil
	}
	if IsKeyword(token) {
		return nil
	}
	limit := keywordSuggestionDistanceCap
	if perLen := maxDistance(len(token)); perLen < limit {
		limit = perLen
	}
	return suggestWithDistance(token, keywordCandidates(), pos, limit, damerauLevenshtein, ReasonDamerauLevenshtein, true /* preferSameLen */)
}

// damerauLevenshtein returns the edit distance between a and b under
// the Damerau-Levenshtein metric: like classic Levenshtein, but a
// transposition of two adjacent characters counts as one edit instead
// of two. This matters for keyword typos where the most common
// mistake is swapping adjacent letters ("FORM" → "FROM", "TBALE" →
// "TABLE"); under classic Levenshtein those are distance 2, which
// loses to a single-letter deletion that lands on an unrelated
// keyword (e.g. "FORM" → "FOR").
//
// The implementation is the optimal-string-alignment (OSA) variant —
// each substring may participate in at most one transposition. That
// is sufficient for human typo correction; the full Damerau metric
// (which also handles arbitrarily nested transpositions) costs more
// without measurable benefit for short SQL tokens.
//
// Inputs are walked byte-wise; callers fold case before invoking.
func damerauLevenshtein(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	// Three rolling rows: prevPrev (i-2), prev (i-1), curr (i).
	// prevPrev is needed to spot the (a[i-1] == b[j-2] && a[i-2] ==
	// b[j-1]) transposition cell.
	prevPrev := make([]int, len(b)+1)
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			best := min(
				prev[j]+1,      // deletion
				curr[j-1]+1,    // insertion
				prev[j-1]+cost, // substitution
			)
			if i >= 2 && j >= 2 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				if t := prevPrev[j-2] + 1; t < best {
					best = t
				}
			}
			curr[j] = best
		}
		// Rotate rows: old prev becomes prevPrev (row i-1 is the
		// next iteration's i-2), old curr becomes prev (row i is
		// the next iteration's i-1), and the old prevPrev slot is
		// recycled as the next curr scratch — every cell will be
		// overwritten by curr[0]=i and the inner-loop assignments
		// before it is read again.
		prevPrev, prev, curr = prev, curr, prevPrev
	}
	return prev[len(b)]
}

// lowerASCII lower-cases an ASCII identifier without allocating when
// the input is already lower-case. Non-ASCII bytes are passed through
// unchanged (no Unicode case folding), which is the desired behavior
// for the IsKeyword/SuggestKeyword consumers: SQL keyword tokens are
// ASCII by construction in lexbase.KeywordsCategories, so any
// non-ASCII byte makes the lookup correctly miss. IsKeyword is
// exported and may receive arbitrary input from future callers — the
// "non-ASCII bytes pass through" contract is what keeps the lookup
// well-defined for them.
func lowerASCII(s string) string {
	needs := false
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			needs = true
			break
		}
	}
	if !needs {
		return s
	}
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}
