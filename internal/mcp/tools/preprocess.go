// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package tools

import (
	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/sqlformat"
)

// preprocessSQL strips cockroach sql REPL prompts from sql and, when
// any prompt bytes were removed, appends an output.CodeInputPreprocessed
// WARNING to env so the modification is visible to callers. The handler
// should pass the returned StripResult.Stripped through to every
// downstream parser/safety/semcheck/cluster step, and the original sql
// to translateErrorPositions whenever a step appends to env.Errors.
//
// The function is a hot-path no-op when the input contains no prompts:
// no allocation beyond what sqlformat.StripShellPromptsWithMap already
// does, and env is left untouched.
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

// translateErrorPositions remaps Position fields on every error in
// env.Errors[fromIdx:] from stripped-SQL coordinates back to the
// original input. fromIdx is the value of len(env.Errors) captured
// immediately before the diagnostic-producing step (parse / safety /
// semcheck / cluster); errors appended earlier (e.g. an
// input_preprocessed warning or a target_version_mismatch warning) are
// left alone because their positions, when present, are already in
// the correct frame.
//
// The call is a no-op when the stripper did not remove anything, so
// handlers can call it unconditionally after each diagnostic step.
//
// Invariant: fromIdx must be captured BEFORE the diagnostic step that
// produced the new env.Errors entries. Capturing fromIdx after the
// step would silently no-op (the loop runs zero times) and ship
// stripped-buffer Position values to the caller — exactly the bug this
// helper exists to prevent. There is no compile-time check; callers
// must keep `before := len(env.Errors)` adjacent to the step.
func translateErrorPositions(
	env *output.Envelope, fromIdx int, originalSQL string, strip sqlformat.StripResult,
) {
	if !strip.Removed {
		return
	}
	for i := fromIdx; i < len(env.Errors); i++ {
		env.Errors[i].Position = diag.AdjustPosition(env.Errors[i].Position, originalSQL, strip.Translate)
	}
}
