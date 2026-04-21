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
	"strings"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/pgwire/pgerror"

	"github.com/spilchen/sql-ai-tools/internal/output"
)

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

	return output.Error{
		Code:     code,
		Severity: output.Severity(sev),
		Message:  err.Error(),
		Position: ExtractPosition(detail, fullSQL),
		Category: CategoryForCode(code),
	}
}
