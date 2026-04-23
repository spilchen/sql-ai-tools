// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package conn

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSplitTableName covers every branch of the user-supplied
// table-name parser. The function is reachable by every live
// describe call (CLI and MCP), so a regression in any branch is a
// production-visible bug — yet none of the integration tests
// exercise the malformed cases (empty, whitespace, leading/trailing
// dot, three-part). Pure-function unit coverage here makes those
// branches falsifiable without a cluster.
func TestSplitTableName(t *testing.T) {
	tests := []struct {
		name           string
		input          string
		expectedSchema string
		expectedTable  string
		expectedErr    string
	}{
		{
			name:          "unqualified",
			input:         "users",
			expectedTable: "users",
		},
		{
			name:           "qualified",
			input:          "public.users",
			expectedSchema: "public",
			expectedTable:  "users",
		},
		{
			name:          "leading and trailing whitespace trimmed",
			input:         "  users  ",
			expectedTable: "users",
		},
		{
			name:           "qualified with mixed-case identifiers preserved",
			input:          "App.Orders",
			expectedSchema: "App",
			expectedTable:  "Orders",
		},
		{
			name:        "empty input rejected",
			input:       "",
			expectedErr: "table name must not be empty",
		},
		{
			name:        "whitespace-only rejected",
			input:       "   ",
			expectedErr: "table name must not be empty",
		},
		{
			name:        "three-part rejected with database guidance",
			input:       "db.public.users",
			expectedErr: "database is fixed by the DSN",
		},
		{
			name:        "leading dot rejected",
			input:       ".users",
			expectedErr: "schema and table must both be non-empty",
		},
		{
			name:        "trailing dot rejected",
			input:       "public.",
			expectedErr: "schema and table must both be non-empty",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			schema, table, err := splitTableName(tc.input)
			if tc.expectedErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.expectedErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expectedSchema, schema)
			require.Equal(t, tc.expectedTable, table)
		})
	}
}
