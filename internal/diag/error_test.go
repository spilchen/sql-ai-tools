// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package diag

import (
	"errors"
	"fmt"
	"testing"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/output"
)

func TestFromParseError(t *testing.T) {
	tests := []struct {
		name                  string
		sql                   string
		expectedCode          string
		expectedSeverity      output.Severity
		expectedMsgSub        string
		expectedPos           *output.Position
		expectedCategory      string
		expectedSuggestion    string
		expectedSuggestionEnd int
	}{
		{
			name:             "syntax error at EOF carries no keyword suggestion",
			sql:              "SELECT FROM",
			expectedCode:     "42601",
			expectedSeverity: output.SeverityError,
			expectedMsgSub:   "syntax error",
			expectedPos: &output.Position{
				Line:       1,
				Column:     12,
				ByteOffset: 11,
			},
			expectedCategory: "syntax_error",
		},
		{
			name:             "misspelled keyword at start gets did-you-mean",
			sql:              "SELECTT 1",
			expectedCode:     "42601",
			expectedSeverity: output.SeverityError,
			expectedMsgSub:   "syntax error",
			expectedPos: &output.Position{
				Line:       1,
				Column:     1,
				ByteOffset: 0,
			},
			expectedCategory:      "syntax_error",
			expectedSuggestion:    "select",
			expectedSuggestionEnd: 7,
		},
		{
			// `SELECT * FORM t` is the canonical FORM/FROM demo from
			// issue #162: the parser errors at FORM (column 10), and
			// keyword Levenshtein flips it to FROM. We deliberately
			// avoid `SELECT 1 FORM t` because (as of cockroachdb-parser
			// v0.26.2) the parser silently accepts FORM as a bare
			// column alias and errors at `t` instead, sliding the
			// position past the typo. If a future parser version
			// rejects bare-alias keywords, that test would also work,
			// but we don't want a parser bump to silently break the
			// position assertion below.
			name:             "misspelled keyword mid-statement gets did-you-mean",
			sql:              "SELECT * FORM t",
			expectedCode:     "42601",
			expectedSeverity: output.SeverityError,
			expectedMsgSub:   "syntax error",
			expectedPos: &output.Position{
				Line:       1,
				Column:     10,
				ByteOffset: 9,
			},
			expectedCategory:      "syntax_error",
			expectedSuggestion:    "from",
			expectedSuggestionEnd: 13,
		},
		{
			// `INSERT FROM t` errors at FROM (byte 7) because INSERT
			// requires INTO. FROM is itself a keyword, so the
			// suggestion path must not fire — otherwise we'd hand the
			// agent a "did you mean FROM?" reply for a token that
			// already says FROM. Earlier we tried `SELECT FROM t`,
			// but (as of cockroachdb-parser v0.26.2) it parses cleanly
			// with an empty projection list (Postgres-compat). A
			// future parser version may tighten that, in which case
			// either fixture would do.
			name:             "exact-match keyword token suppresses suggestion",
			sql:              "INSERT FROM t",
			expectedCode:     "42601",
			expectedSeverity: output.SeverityError,
			expectedMsgSub:   "syntax error",
			expectedPos: &output.Position{
				Line:       1,
				Column:     8,
				ByteOffset: 7,
			},
			expectedCategory: "syntax_error",
		},
		{
			name:             "multi-statement error on second",
			sql:              "SELECT 1;\nSELECT FROM",
			expectedCode:     "42601",
			expectedSeverity: output.SeverityError,
			expectedMsgSub:   "syntax error",
			expectedPos: &output.Position{
				Line:       2,
				Column:     12,
				ByteOffset: 21,
			},
			expectedCategory: "syntax_error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parser.Parse(tc.sql)
			require.Error(t, err)

			diagErr := FromParseError(err, tc.sql)
			require.Equal(t, tc.expectedCode, diagErr.Code)
			require.Equal(t, tc.expectedSeverity, diagErr.Severity)
			require.Contains(t, diagErr.Message, tc.expectedMsgSub)

			require.Equal(t, tc.expectedCategory, diagErr.Category)

			if tc.expectedPos == nil {
				require.Nil(t, diagErr.Position)
			} else {
				require.NotNil(t, diagErr.Position)
				require.Equal(t, *tc.expectedPos, *diagErr.Position)
			}

			if tc.expectedSuggestion == "" {
				require.Nil(t, diagErr.Suggestions, "expected no keyword suggestions")
				return
			}
			require.NotEmpty(t, diagErr.Suggestions)
			require.Equal(t, tc.expectedSuggestion, diagErr.Suggestions[0].Replacement)
			require.Equal(t, tc.expectedPos.ByteOffset, diagErr.Suggestions[0].Range.Start)
			require.Equal(t, tc.expectedSuggestionEnd, diagErr.Suggestions[0].Range.End)
		})
	}
}

// TestIdentifierAt covers identifierAt's boundary conditions
// directly. The TestFromParseError table only exercises the happy
// paths through real parser errors; this table pins the edge cases
// that would otherwise go untested (offset at EOF, offset past EOF,
// offset in whitespace, offset on a non-letter, multibyte byte at
// offset, multi-line input).
func TestIdentifierAt(t *testing.T) {
	tests := []struct {
		name     string
		fullSQL  string
		offset   int
		nilPos   bool
		expected string
	}{
		{name: "nil position returns empty", fullSQL: "SELECT 1", nilPos: true, expected: ""},
		{name: "offset at EOF returns empty", fullSQL: "SELECT", offset: 6, expected: ""},
		{name: "offset past EOF returns empty", fullSQL: "SELECT", offset: 99, expected: ""},
		{name: "negative offset returns empty", fullSQL: "SELECT", offset: -1, expected: ""},
		{name: "offset in whitespace returns empty", fullSQL: "SELECT FROM", offset: 6, expected: ""},
		{name: "offset on punctuation returns empty", fullSQL: "SELECT *", offset: 7, expected: ""},
		{name: "offset on digit returns empty", fullSQL: "SELECT 3LECT", offset: 7, expected: ""},
		{name: "non-ASCII leading byte returns empty", fullSQL: "SELECT é", offset: 7, expected: ""},
		{name: "identifier truncates at non-ASCII byte", fullSQL: "café", offset: 0, expected: "caf"},
		{name: "ASCII identifier at start", fullSQL: "SELECTT 1", offset: 0, expected: "SELECTT"},
		{name: "ASCII identifier mid-statement", fullSQL: "SELECT * FORM t", offset: 9, expected: "FORM"},
		{name: "multi-line: offset in second line", fullSQL: "SELECT 1;\nSELCT 2", offset: 10, expected: "SELCT"},
		{name: "underscore-led identifier", fullSQL: "_priv_col", offset: 0, expected: "_priv_col"},
		{name: "identifier with digits in middle", fullSQL: "abc123def", offset: 0, expected: "abc123def"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var pos *output.Position
			if !tc.nilPos {
				pos = &output.Position{ByteOffset: tc.offset}
			}
			require.Equal(t, tc.expected, identifierAt(tc.fullSQL, pos))
		})
	}
}

func TestFromClusterError(t *testing.T) {
	tests := []struct {
		name             string
		err              error
		fullSQL          string
		expectedCode     string
		expectedSeverity output.Severity
		expectedMessage  string
		expectedCategory string
		expectedPos      *output.Position
	}{
		{
			name: "undefined table populates code, message, and category",
			err: &pgconn.PgError{
				SeverityUnlocalized: "ERROR",
				Code:                "42P01",
				Message:             `relation "doesnotexist" does not exist`,
			},
			expectedCode:     "42P01",
			expectedSeverity: output.SeverityError,
			expectedMessage:  `relation "doesnotexist" does not exist`,
			expectedCategory: "unknown_table",
		},
		{
			name: "permission denied maps to permission_denied category",
			err: &pgconn.PgError{
				SeverityUnlocalized: "ERROR",
				Code:                "42501",
				Message:             "permission denied for table users",
			},
			expectedCode:     "42501",
			expectedSeverity: output.SeverityError,
			expectedMessage:  "permission denied for table users",
			expectedCategory: "permission_denied",
		},
		{
			name: "connection failure falls back to class-level connection_error",
			err: &pgconn.PgError{
				SeverityUnlocalized: "FATAL",
				Code:                "08006",
				Message:             "connection terminated",
			},
			expectedCode:     "08006",
			expectedSeverity: output.SeverityFatal,
			expectedMessage:  "connection terminated",
			expectedCategory: "connection_error",
		},
		{
			name: "wrapped pgwire error is unwrapped via errors.As",
			err: fmt.Errorf("query cluster info: %w", &pgconn.PgError{
				SeverityUnlocalized: "ERROR",
				Code:                "42P01",
				Message:             "relation does not exist",
			}),
			expectedCode:     "42P01",
			expectedSeverity: output.SeverityError,
			expectedMessage:  "relation does not exist",
			expectedCategory: "unknown_table",
		},
		{
			name: "position populated when fullSQL and Position supplied",
			err: &pgconn.PgError{
				SeverityUnlocalized: "ERROR",
				Code:                "42P01",
				Message:             "relation does not exist",
				Position:            15,
			},
			fullSQL:          "SELECT * FROM doesnotexist",
			expectedCode:     "42P01",
			expectedSeverity: output.SeverityError,
			expectedMessage:  "relation does not exist",
			expectedCategory: "unknown_table",
			expectedPos: &output.Position{
				Line:       1,
				Column:     15,
				ByteOffset: 14,
			},
		},
		{
			name: "multibyte identifier shifts byte offset past character index",
			err: &pgconn.PgError{
				SeverityUnlocalized: "ERROR",
				Code:                "42703",
				Message:             "column does not exist",
				// 'é' is 2 bytes in UTF-8; the column 'x' is the 29th
				// character (1-based) but the 30th byte (1-based).
				// Without char→byte translation Column would be 29.
				Position: 29,
			},
			fullSQL:          `SELECT * FROM "tbl_é" WHERE x=1`,
			expectedCode:     "42703",
			expectedSeverity: output.SeverityError,
			expectedMessage:  "column does not exist",
			expectedCategory: "unknown_column",
			expectedPos: &output.Position{
				Line:       1,
				Column:     30,
				ByteOffset: 29,
			},
		},
		{
			name: "position omitted when Position is zero with fullSQL set",
			err: &pgconn.PgError{
				SeverityUnlocalized: "ERROR",
				Code:                "42P01",
				Message:             "relation does not exist",
				Position:            0,
			},
			fullSQL:          "SELECT * FROM doesnotexist",
			expectedCode:     "42P01",
			expectedSeverity: output.SeverityError,
			expectedMessage:  "relation does not exist",
			expectedCategory: "unknown_table",
		},
		{
			name: "position omitted when fullSQL is empty",
			err: &pgconn.PgError{
				SeverityUnlocalized: "ERROR",
				Code:                "42P01",
				Message:             "relation does not exist",
				Position:            15,
			},
			expectedCode:     "42P01",
			expectedSeverity: output.SeverityError,
			expectedMessage:  "relation does not exist",
			expectedCategory: "unknown_table",
		},
		{
			name: "empty severity defaults to ERROR",
			err: &pgconn.PgError{
				Code:    "42P01",
				Message: "relation does not exist",
			},
			expectedCode:     "42P01",
			expectedSeverity: output.SeverityError,
			expectedMessage:  "relation does not exist",
			expectedCategory: "unknown_table",
		},
		{
			name: "localized severity used when unlocalized is empty",
			err: &pgconn.PgError{
				Severity: "WARNING",
				Code:     "01000",
				Message:  "warning from cluster",
			},
			expectedCode:     "01000",
			expectedSeverity: output.SeverityWarning,
			expectedMessage:  "warning from cluster",
		},
		{
			name:             "non-pgwire error falls back to internal_error",
			err:              errors.New("dial tcp 127.0.0.1:1: connect: connection refused"),
			expectedCode:     "internal_error",
			expectedSeverity: output.SeverityError,
			expectedMessage:  "dial tcp 127.0.0.1:1: connect: connection refused",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			diagErr := FromClusterError(tc.err, tc.fullSQL)
			require.Equal(t, tc.expectedCode, diagErr.Code)
			require.Equal(t, tc.expectedSeverity, diagErr.Severity)
			require.Equal(t, tc.expectedMessage, diagErr.Message)
			require.Equal(t, tc.expectedCategory, diagErr.Category)

			if tc.expectedPos == nil {
				require.Nil(t, diagErr.Position)
			} else {
				require.NotNil(t, diagErr.Position)
				require.Equal(t, *tc.expectedPos, *diagErr.Position)
			}
		})
	}
}
