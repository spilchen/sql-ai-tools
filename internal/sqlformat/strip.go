// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sqlformat

import (
	"regexp"
	"sort"
	"strings"
)

// primaryPromptRE matches a cockroach sql REPL primary prompt at the
// start of a line. The structure is:
//
//	<user>@<host-info>><SP?>
//
// where:
//   - user is a SQL identifier-shaped name (root, app_user, marc.smith, …);
//   - host-info is any non-space, non-'>' run that precedes the closing
//     '>' (covers host:port/db, host/db, empty host as in
//     ":26257/defaultdb", and the OPEN/ERROR connection-state word that
//     newer cockroach sql builds insert before '>', e.g.
//     "root@:26257/defaultdb OPEN>").
//
// The optional trailing space following '>' is the single space the
// REPL prints between prompt and command.
var primaryPromptRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*@[^\s>]*(?:[ \t]+[A-Za-z]+)?> ?`)

// continuationPromptRE matches a continuation prompt anchored at the
// start of a line, allowing the leading whitespace the REPL inserts to
// right-align '>' under the primary prompt:
//
//	-> SELECT 2;
//
// The optional trailing space mirrors primaryPromptRE.
var continuationPromptRE = regexp.MustCompile(`^[ \t]*-> ?`)

// StripShellPrompts removes cockroach sql REPL prompt prefixes from
// pasted input. Two prompt forms are recognized:
//
//	<user>@<host-info>><SP>     // primary prompt (any user)
//	<pad>-><SP>                 // continuation prompt (right-aligned)
//
// Continuation lines are stripped only after a primary prompt has
// already been seen earlier in the input. This guards SQL-only pastes
// that legitimately contain the JSON field-access operator '->' at line
// start (e.g. "  -> 'key'") from being mangled. A transcript paste
// always starts with a primary prompt, so the guard does not interfere
// with the intended use case.
//
// The function is a no-op when the input contains no prompts. Lines
// without a recognized prompt — including blank lines and lines inside
// multi-line string literals — pass through unchanged.
//
// Limitation: a multi-line string literal whose interior line happens
// to begin with a primary-prompt-shaped sequence will be incorrectly
// stripped. This is exceedingly rare in REPL transcripts and not worth
// the complexity of full lexer-aware line classification at this layer.
func StripShellPrompts(sql string) string {
	return StripShellPromptsWithMap(sql).Stripped
}

// StripResult is the value produced by StripShellPromptsWithMap. It pairs
// the stripped SQL with an offset map that lets callers translate byte
// offsets in the stripped buffer back to the original input — needed so
// parser/cluster error positions can be reported in the user's original
// coordinates after the stripper has shortened earlier lines.
//
// Removed reports whether any prompt bytes were dropped. When false,
// Stripped is byte-identical to the function input and Translate is the
// identity, so handlers can safely treat the no-op case the same as the
// strip-applied case.
//
// segments is the per-removal cumulative-delta table consumed by
// Translate. Each entry records the offset in Stripped where a stretch
// of "no further bytes removed" begins, plus the total number of bytes
// dropped from the original before that offset. Lookup is a binary
// search for the largest StrippedStart ≤ query offset, so translation
// is O(log N) in the number of stripped lines.
type StripResult struct {
	Stripped string
	Removed  bool
	segments []stripSegment
}

// stripSegment is one entry in StripResult.segments. Entries are kept
// sorted by StrippedStart ascending so Translate can binary-search them.
type stripSegment struct {
	// StrippedStart is the 0-based byte offset in Stripped at which
	// the post-removal text begins.
	StrippedStart int
	// CumDelta is the cumulative number of bytes removed from the
	// original input prior to StrippedStart. The translation rule is
	// originalOffset = strippedOffset + CumDelta.
	CumDelta int
}

// StripShellPromptsWithMap behaves like StripShellPrompts but also
// returns an offset map. Use this when you intend to surface error
// positions back to the caller, so a "syntax error at column 30" in the
// stripped buffer can be reported as the matching column in the
// original paste. For pure formatting / display use cases the simpler
// StripShellPrompts is sufficient.
func StripShellPromptsWithMap(sql string) StripResult {
	if sql == "" {
		return StripResult{Stripped: sql}
	}

	lines := strings.SplitAfter(sql, "\n")
	var (
		buf      strings.Builder
		segments []stripSegment
		// cumDelta tracks bytes removed from the original input so far.
		// It is appended into a new segment whenever it changes, anchored
		// at the post-removal offset in the stripped output.
		cumDelta    int
		seenPrimary bool
	)
	buf.Grow(len(sql))

	recordSegment := func(promptLen int) {
		cumDelta += promptLen
		segments = append(segments, stripSegment{
			StrippedStart: buf.Len(),
			CumDelta:      cumDelta,
		})
	}

	for _, line := range lines {
		if loc := primaryPromptRE.FindStringIndex(line); loc != nil {
			seenPrimary = true
			recordSegment(loc[1])
			buf.WriteString(line[loc[1]:])
			continue
		}
		if seenPrimary {
			if loc := continuationPromptRE.FindStringIndex(line); loc != nil {
				recordSegment(loc[1])
				buf.WriteString(line[loc[1]:])
				continue
			}
		}
		buf.WriteString(line)
	}

	return StripResult{
		Stripped: buf.String(),
		Removed:  len(segments) > 0,
		segments: segments,
	}
}

// Translate maps a 0-based byte offset in r.Stripped to the matching
// 0-based byte offset in the original input. Negative offsets clamp to
// 0; offsets past len(r.Stripped) clamp to len(original).
//
// Translation works by binary-searching segments for the largest
// StrippedStart ≤ strippedOffset and adding its CumDelta. When no
// segments were produced (no prompts removed), Translate is the
// identity.
func (r StripResult) Translate(strippedOffset int) int {
	if strippedOffset < 0 {
		strippedOffset = 0
	}
	if strippedOffset > len(r.Stripped) {
		strippedOffset = len(r.Stripped)
	}
	if len(r.segments) == 0 {
		return strippedOffset
	}

	// sort.Search finds the first segment whose StrippedStart >
	// strippedOffset; the segment one before that is the largest
	// StrippedStart ≤ strippedOffset. When idx == 0, no segment
	// applies (the offset falls before any removal happened).
	idx := sort.Search(len(r.segments), func(i int) bool {
		return r.segments[i].StrippedStart > strippedOffset
	})
	if idx == 0 {
		return strippedOffset
	}
	return strippedOffset + r.segments[idx-1].CumDelta
}
