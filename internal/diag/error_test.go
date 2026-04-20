// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package diag

import (
	"testing"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
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

			if tc.expectedPos == nil {
				require.Nil(t, diagErr.Position)
			} else {
				require.NotNil(t, diagErr.Position)
				require.Equal(t, *tc.expectedPos, *diagErr.Position)
			}
		})
	}
}
