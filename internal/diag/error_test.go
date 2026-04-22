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
		name             string
		sql              string
		expectedCode     string
		expectedSeverity output.Severity
		expectedMsgSub   string
		expectedPos      *output.Position
		expectedCategory string
	}{
		{
			name:             "syntax error at EOF",
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
			name:             "misspelled keyword",
			sql:              "SELECTT 1",
			expectedCode:     "42601",
			expectedSeverity: output.SeverityError,
			expectedMsgSub:   "syntax error",
			expectedPos: &output.Position{
				Line:       1,
				Column:     1,
				ByteOffset: 0,
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
