// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package diag

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/spilchen/sql-ai-tools/internal/output"
)

// maxSuggestions caps the number of structured fixes returned per
// error. Three is enough to cover the common "1-2 close matches"
// pattern without flooding the response with low-confidence noise.
const maxSuggestions = 3

// Suggest returns up to three "did you mean?" fix suggestions for
// the misspelled token, ranked by Levenshtein edit distance against
// candidates. Suggestions carry a Reason prefix of ReasonLevenshtein.
//
// Suggestions are filtered by a length-scaled threshold: short names
// permit fewer edits (e.g. distance 2 between "id" and "od" is too
// loose to be useful), longer names tolerate up to three. The full
// rule lives in maxDistance.
//
// Returns nil when:
//   - misspelled is empty;
//   - pos is nil (no byte range can be computed, so the suggestion
//     cannot be applied programmatically by the agent);
//   - candidates is empty;
//   - no candidate is within the length-scaled threshold.
//
// Per-candidate filtering: empty strings and case-insensitive exact
// matches against misspelled are skipped (they don't represent a
// useful fix). If every candidate is filtered, the function returns
// nil along the "no candidate within threshold" path.
//
// The returned Range covers [pos.ByteOffset, pos.ByteOffset +
// len(misspelled)) in the original SQL input. Confidence is in [0, 1]
// and rounded to two decimals.
//
// candidates is read but not retained, and Suggestion.Replacement
// borrows the underlying string from candidates (Go strings are
// immutable, so this is safe for typical []string callers).
func Suggest(misspelled string, candidates []string, pos *output.Position) []output.Suggestion {
	if misspelled == "" {
		return nil
	}
	return suggestWithDistance(misspelled, candidates, pos, maxDistance(len(misspelled)), levenshtein, ReasonLevenshtein, false /* preferSameLen */)
}

// ReasonLevenshtein and ReasonDamerauLevenshtein are the metric
// prefixes embedded in the output.Suggestion.Reason field. Each
// emitted Reason has the shape "<prefix>_<distance>" — for example
// "levenshtein_distance_1" or "damerau_levenshtein_distance_2" — so
// callers branching on the metric should compare with
// strings.HasPrefix(s.Reason, ReasonLevenshtein+"_") rather than
// identity. The trailing distance integer is comparable only within
// the same metric: a Damerau-Levenshtein distance of 1 can
// correspond to a Levenshtein distance of 2 when the typo is an
// adjacent transposition.
const (
	ReasonLevenshtein        = "levenshtein_distance"
	ReasonDamerauLevenshtein = "damerau_levenshtein_distance"
)

// suggestWithDistance is the lowest-level "did you mean?" ranker. It
// is shared by the Levenshtein path (Suggest, used by semcheck for
// unknown table/function/column names) and the Damerau-Levenshtein
// path (SuggestKeyword, used by FromParseError for keyword typos).
//
// dist is the edit-distance function applied to the lower-cased
// misspelled/candidate pair; callers pick Levenshtein or
// Damerau-Levenshtein. reasonPrefix names the metric in the emitted
// Suggestion.Reason field (e.g. ReasonLevenshtein → "levenshtein_
// distance_1") so downstream agents can tell the metrics apart.
// preferSameLen, when true, breaks ties between candidates at the
// same distance by preferring those whose length matches the
// misspelled token — this biases toward substitution/transposition
// fixes over insertion/deletion ones, which is the right call when
// the metric is Damerau and adjacent swaps are the dominant typo.
//
// limit is the inclusive distance cap; candidates farther than limit
// are skipped (with a length pre-filter to avoid the DP). Callers
// passing limit <= 0 get nil.
func suggestWithDistance(
	misspelled string,
	candidates []string,
	pos *output.Position,
	limit int,
	dist func(a, b string) int,
	reasonPrefix string,
	preferSameLen bool,
) []output.Suggestion {
	if misspelled == "" || pos == nil || len(candidates) == 0 || limit <= 0 {
		return nil
	}

	type scored struct {
		name     string
		distance int
	}
	var hits []scored
	for _, cand := range candidates {
		if cand == "" || strings.EqualFold(cand, misspelled) {
			continue
		}
		// Length pre-filter: a difference larger than the threshold
		// means at least that many edits are required, so skip the
		// O(n*m) DP.
		if absDiff(len(cand), len(misspelled)) > limit {
			continue
		}
		d := dist(strings.ToLower(misspelled), strings.ToLower(cand))
		if d > limit {
			continue
		}
		hits = append(hits, scored{name: cand, distance: d})
	}
	if len(hits) == 0 {
		return nil
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].distance != hits[j].distance {
			return hits[i].distance < hits[j].distance
		}
		if preferSameLen {
			iSame := len(hits[i].name) == len(misspelled)
			jSame := len(hits[j].name) == len(misspelled)
			if iSame != jSame {
				return iSame
			}
		}
		return hits[i].name < hits[j].name
	})
	if len(hits) > maxSuggestions {
		hits = hits[:maxSuggestions]
	}

	rng := output.Range{
		Start: pos.ByteOffset,
		End:   pos.ByteOffset + len(misspelled),
	}
	out := make([]output.Suggestion, len(hits))
	for i, h := range hits {
		out[i] = output.Suggestion{
			Replacement: h.name,
			Range:       rng,
			Confidence:  confidence(h.distance, max(len(misspelled), len(h.name))),
			Reason:      fmt.Sprintf("%s_%d", reasonPrefix, h.distance),
		}
	}
	return out
}

// maxDistance returns the largest edit distance accepted for a
// misspelled token of the given length. The rule scales with name
// length so short tokens don't get spurious "fixes":
//
//	len ≤ 3 → 1
//	len ≤ 6 → 2
//	len ≥ 7 → 3
//
// Three is the cap; beyond that the candidate is too dissimilar to
// be a useful "did you mean?" hit and would dilute the signal.
func maxDistance(nameLen int) int {
	switch {
	case nameLen <= 3:
		return 1
	case nameLen <= 6:
		return 2
	default:
		return 3
	}
}

// confidence maps an edit distance to a [0, 1] score, rounded to two
// decimals. The formula `1 - distance/maxLen` is a heuristic, not a
// contract — agents that branch on confidence should treat the
// numeric value as an ordering, not an absolute probability.
func confidence(distance, maxLen int) float64 {
	if maxLen == 0 {
		return 0
	}
	c := 1.0 - float64(distance)/float64(maxLen)
	return math.Round(c*100) / 100
}

// levenshtein returns the edit distance between a and b using a
// two-row DP. Both inputs are walked as bytes; callers fold case
// before invoking when ASCII case-insensitive comparison is desired.
// Multi-byte UTF-8 sequences are compared byte-wise — adequate for
// SQL identifiers, which are predominantly ASCII.
func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
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
			curr[j] = min(
				prev[j]+1,      // deletion
				curr[j-1]+1,    // insertion
				prev[j-1]+cost, // substitution
			)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func absDiff(a, b int) int {
	if a > b {
		return a - b
	}
	return b - a
}
