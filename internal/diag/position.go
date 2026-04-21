// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package diag

import (
	"strings"

	"github.com/spilchen/sql-ai-tools/internal/output"
)

// detailPrefix is the fixed header that PopulateErrorDetails (in the
// CockroachDB parser) prepends to every Detail string. The remainder
// is the statement SQL through the end of the line containing the
// error token, followed by a caret line.
const detailPrefix = "source SQL:\n"

// ExtractPosition parses a pgerror Detail string produced by the
// CockroachDB parser's PopulateErrorDetails and computes a 1-based
// line/column Position relative to fullSQL.
//
// The Detail format is:
//
//	source SQL:\n<stmt_sql_up_to_error_line>\n<spaces>^
//
// For multi-statement input the Detail contains only the failing
// statement's SQL fragment. ExtractPosition locates that fragment
// within fullSQL via strings.LastIndex and adjusts the offset.
// LastIndex is used because the parser processes statements
// left-to-right and fails on the last attempted one; if the same
// fragment appears earlier (e.g. inside a valid statement), the
// last occurrence is the correct match.
//
// Returns nil when detail is empty, lacks the expected prefix, or
// does not contain a caret line.
func ExtractPosition(detail, fullSQL string) *output.Position {
	if !strings.HasPrefix(detail, detailPrefix) {
		return nil
	}
	body := detail[len(detailPrefix):]

	lastNL := strings.LastIndex(body, "\n")
	if lastNL < 0 {
		return nil
	}
	caretLine := body[lastNL+1:]
	stmtFragment := body[:lastNL]

	caretCol := caretColumn(caretLine)
	if caretCol < 0 {
		return nil
	}

	// The stmtFragment may be multi-line (parser preserves newlines).
	// The caret column is relative to the last line of stmtFragment.
	lastStmtNL := strings.LastIndex(stmtFragment, "\n")
	var stmtByteOffset int
	if lastStmtNL < 0 {
		stmtByteOffset = caretCol
	} else {
		stmtByteOffset = lastStmtNL + 1 + caretCol
	}

	// Locate the fragment in the full input so multi-statement
	// positions are reported relative to the complete SQL string.
	stmtOffset := strings.LastIndex(fullSQL, stmtFragment)
	if stmtOffset < 0 {
		return nil
	}
	fullByteOffset := stmtOffset + stmtByteOffset

	line, col := lineColumn(fullSQL, fullByteOffset)
	return &output.Position{
		Line:       line,
		Column:     col,
		ByteOffset: fullByteOffset,
	}
}

// caretColumn returns the 0-based column indicated by a caret line
// (e.g. "       ^"). It counts leading spaces before the '^'. Returns
// -1 if the line does not contain a caret.
func caretColumn(line string) int {
	idx := strings.IndexByte(line, '^')
	if idx < 0 {
		return -1
	}
	return idx
}

// PositionFromByteOffset converts a 0-based byte offset within fullSQL
// into a 1-based line/column Position. Returns nil if byteOffset is
// negative.
func PositionFromByteOffset(fullSQL string, byteOffset int) *output.Position {
	if byteOffset < 0 {
		return nil
	}
	line, col := lineColumn(fullSQL, byteOffset)
	return &output.Position{
		Line:       line,
		Column:     col,
		ByteOffset: byteOffset,
	}
}

// lineColumn converts a 0-based byte offset within sql into a 1-based
// line and column. The column counts bytes from the last newline (or
// start of string), not runes, matching the CockroachDB parser's
// convention.
func lineColumn(sql string, byteOffset int) (line, col int) {
	if byteOffset > len(sql) {
		byteOffset = len(sql)
	}
	prefix := sql[:byteOffset]
	line = strings.Count(prefix, "\n") + 1
	lastNL := strings.LastIndex(prefix, "\n")
	if lastNL < 0 {
		col = byteOffset + 1
	} else {
		col = byteOffset - lastNL
	}
	return line, col
}
