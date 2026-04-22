// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sqlformat

import (
	"regexp"
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
	if sql == "" {
		return sql
	}

	lines := strings.SplitAfter(sql, "\n")
	var (
		buf         strings.Builder
		seenPrimary bool
	)
	buf.Grow(len(sql))

	for _, line := range lines {
		if loc := primaryPromptRE.FindStringIndex(line); loc != nil {
			seenPrimary = true
			buf.WriteString(line[loc[1]:])
			continue
		}
		if seenPrimary {
			if loc := continuationPromptRE.FindStringIndex(line); loc != nil {
				buf.WriteString(line[loc[1]:])
				continue
			}
		}
		buf.WriteString(line)
	}
	return buf.String()
}
