// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package diag converts CockroachDB parser errors into the structured
// output.Error type that the CLI envelope exposes to agents. The
// enrichment has two layers:
//
//   - Position extraction (position.go): parses the pgerror Detail
//     caret string to compute a 1-based line/column/byte_offset
//     relative to the full SQL input.
//
//   - Error mapping (this file): extracts the SQLSTATE code, severity,
//     and human-readable message from the parser's error chain via
//     pgerror, then attaches the position.
package diag

import (
	"errors"
	"strings"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/pgwire/pgerror"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/spilchen/sql-ai-tools/internal/output"
)

// FromTypeError converts a type-check error into a structured
// output.Error. exprText is the formatted expression that failed
// (used to locate the error position within fullSQL via substring
// match). Position is nil when exprText cannot be found.
func FromTypeError(err error, exprText string, fullSQL string) output.Error {
	code := pgerror.GetPGCode(err).String()

	sev := pgerror.GetSeverity(err)
	if sev == "" {
		sev = string(output.SeverityError)
	}

	var pos *output.Position
	if exprText != "" {
		if idx := strings.Index(fullSQL, exprText); idx >= 0 {
			pos = PositionFromByteOffset(fullSQL, idx)
		}
	}

	return output.Error{
		Code:     code,
		Severity: output.Severity(sev),
		Message:  err.Error(),
		Position: pos,
	}
}

// FromParseError converts a Go error returned by parser.Parse into a
// structured output.Error with SQLSTATE code, severity, message, and
// source position. fullSQL is the complete SQL input; it is used to
// compute position relative to the full input when the parser
// operated on a per-statement fragment.
//
// The returned Error always has Code, Severity, and Message populated.
// Position is nil when the error lacks the expected pgerror Detail
// format (e.g. non-parser errors passed by mistake).
func FromParseError(err error, fullSQL string) output.Error {
	code := pgerror.GetPGCode(err).String()

	sev := pgerror.GetSeverity(err)
	if sev == "" {
		sev = string(output.SeverityError)
	}

	flat := pgerror.Flatten(err)

	// Flatten may join multiple detail annotations with "\n--\n".
	// The parser's PopulateErrorDetails is always the first block.
	detail := flat.Detail
	if sep := strings.Index(detail, "\n--\n"); sep >= 0 {
		detail = detail[:sep]
	}

	pos := ExtractPosition(detail, fullSQL)
	return output.Error{
		Code:        code,
		Severity:    output.Severity(sev),
		Message:     err.Error(),
		Position:    pos,
		Category:    CategoryForCode(code),
		Suggestions: SuggestKeyword(identifierAt(fullSQL, pos), pos),
	}
}

// identifierAt returns the ASCII identifier-shaped run that begins at
// pos.ByteOffset in fullSQL, or "" when pos is nil, the offset is out
// of range, or the byte at the offset is not the start of an
// identifier ([A-Za-z_]). This lets FromParseError extract the
// offending token without parsing the human-readable error message —
// the parser sometimes lower-cases the token in `at or near "..."` and
// sometimes uses non-token sentinels like `<EOF>`, so the byte offset
// is the authoritative pointer.
//
// Multi-byte (non-ASCII) leading bytes return "" because keyword
// suggestions only apply to ASCII keyword tokens; quoted identifiers
// or unicode operators don't benefit from a Levenshtein keyword hit.
func identifierAt(fullSQL string, pos *output.Position) string {
	if pos == nil || pos.ByteOffset < 0 || pos.ByteOffset >= len(fullSQL) {
		return ""
	}
	c := fullSQL[pos.ByteOffset]
	if !isIdentStart(c) {
		return ""
	}
	end := pos.ByteOffset + 1
	for end < len(fullSQL) && isIdentCont(fullSQL[end]) {
		end++
	}
	return fullSQL[pos.ByteOffset:end]
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isIdentCont(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// FromClusterError converts a cluster-side error into a structured
// output.Error.
//
// When err's chain contains a *pgconn.PgError (a pgwire protocol
// error from the server), Code, Severity, Message, Category, and
// Position (when fullSQL is supplied and the server reported a
// position) are populated from it. Any error whose chain does not
// contain a *pgconn.PgError falls back to the generic internal_error
// shape so callers always get a single envelope schema.
//
// fullSQL is the originating statement; pass "" when no statement is
// associated with the call. The pgwire Position field is a 1-based
// character index into the original query, so it is only meaningful
// when fullSQL is provided.
func FromClusterError(err error, fullSQL string) output.Error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return output.Error{
			Code:     "internal_error",
			Severity: output.SeverityError,
			Message:  err.Error(),
		}
	}

	// Prefer the unlocalized severity so the wire string is stable
	// across server locales; fall back to the localized field, then
	// to the protocol default.
	sev := pgErr.SeverityUnlocalized
	if sev == "" {
		sev = pgErr.Severity
	}
	if sev == "" {
		sev = string(output.SeverityError)
	}

	var pos *output.Position
	if fullSQL != "" && pgErr.Position > 0 {
		// pgwire Position is a 1-based character index; the rest of
		// this package works in byte offsets, so translate runes to
		// bytes here. ASCII-only SQL is a no-op cost path.
		byteOffset := CharIndexToByteOffset(fullSQL, int(pgErr.Position)-1)
		pos = PositionFromByteOffset(fullSQL, byteOffset)
	}

	return output.Error{
		Code:     pgErr.Code,
		Severity: output.Severity(sev),
		Message:  pgErr.Message,
		Position: pos,
		Category: CategoryForCode(pgErr.Code),
	}
}
