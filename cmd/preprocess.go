// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/sqlformat"
)

// preprocessSQL strips cockroach sql REPL prompts from sql and, when
// any prompt bytes were removed, appends an output.CodeInputPreprocessed
// WARNING to env so the modification is visible to JSON consumers. It
// mirrors the MCP-side preprocessSQL helper so the two surfaces produce
// identical envelope shapes for the same input.
//
// The returned StripResult exposes .Stripped (the SQL the caller should
// pass to the parser, safety check, semcheck, and cluster manager) and
// .Translate (the offset mapper that translateErrorPositions consumes
// after each diagnostic-producing step).
//
// The function is a hot-path no-op when the input contains no prompts.
func preprocessSQL(env *output.Envelope, sql string) sqlformat.StripResult {
	strip := sqlformat.StripShellPromptsWithMap(sql)
	if !strip.Removed {
		return strip
	}
	bytesRemoved := len(sql) - len(strip.Stripped)
	env.Errors = append(env.Errors, output.Error{
		Code:     output.CodeInputPreprocessed,
		Severity: output.SeverityWarning,
		Message:  "input was preprocessed: cockroach sql REPL prompts removed before parsing",
		Context: map[string]any{
			"bytes_removed": bytesRemoved,
		},
	})
	return strip
}

// renderParseErrorTranslated wraps renderParseError with the offset
// translation needed when sql came from a stripped paste. The
// underlying diag.FromParseError computes a Position relative to sql
// (the stripped buffer); this helper re-derives that Position against
// originalSQL so the JSON envelope reports user-visible coordinates.
//
// When strip is the no-op (Removed == false), this delegates straight
// to renderParseError.
func renderParseErrorTranslated(
	r output.Renderer, env output.Envelope, parseErr error, sql, originalSQL string, strip sqlformat.StripResult,
) error {
	if !strip.Removed {
		return renderParseError(r, env, parseErr, sql)
	}
	e := diag.FromParseError(parseErr, sql)
	e.Position = diag.AdjustPosition(e.Position, originalSQL, strip.Translate)
	return renderDiagErrors(r, env, []output.Error{e})
}
