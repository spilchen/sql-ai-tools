// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package tools

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/catalog"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/schemawarn"
)

// findErrorByCode returns the first envelope error whose Code matches
// the given SQLSTATE (or sentinel) and fails the test if none exists.
// Useful when an envelope may carry warnings alongside the error of
// interest and asserting on env.Errors[0] would be brittle.
func findErrorByCode(t *testing.T, errs []output.Error, code string) output.Error {
	t.Helper()
	for _, e := range errs {
		if e.Code == code {
			return e
		}
	}
	require.Failf(t, "envelope error not found", "no error with code %q in %+v", code, errs)
	return output.Error{}
}

// usersDDL and ordersDDL are the canonical fixtures shared across the
// catalog handler tests. Keeping them at package scope means each test
// case only encodes the per-case differences (which schemas to load,
// which table to query) rather than re-stating the DDL.
const (
	usersDDL  = "CREATE TABLE users (id INT PRIMARY KEY, email TEXT NOT NULL UNIQUE)"
	ordersDDL = "CREATE TABLE orders (id INT PRIMARY KEY, user_id INT)"
)

func TestExtractSchemas(t *testing.T) {
	tests := []struct {
		name            string
		args            map[string]any
		mode            schemasRequirement
		expectedToolErr bool
		expectedSources int
		expectedSQL     string
		expectedLabel   string
	}{
		{
			name:            "valid two entries",
			args:            map[string]any{schemasParam: []any{map[string]any{"sql": usersDDL, "label": "u"}, map[string]any{"sql": ordersDDL}}},
			mode:            schemasRequired,
			expectedSources: 2,
		},
		{
			name:            "valid label omitted",
			args:            map[string]any{schemasParam: []any{map[string]any{"sql": usersDDL}}},
			mode:            schemasRequired,
			expectedSources: 1,
			expectedSQL:     usersDDL,
		},
		{
			name:            "valid label provided is preserved verbatim",
			args:            map[string]any{schemasParam: []any{map[string]any{"sql": usersDDL, "label": "  users-schema  "}}},
			mode:            schemasRequired,
			expectedSources: 1,
			expectedLabel:   "  users-schema  ",
		},
		{
			name:            "trims surrounding whitespace from sql",
			args:            map[string]any{schemasParam: []any{map[string]any{"sql": "  " + usersDDL + "\n"}}},
			mode:            schemasRequired,
			expectedSources: 1,
			expectedSQL:     usersDDL,
		},
		{
			name:            "missing required",
			args:            map[string]any{},
			mode:            schemasRequired,
			expectedToolErr: true,
		},
		{
			name:            "missing optional yields nil",
			args:            map[string]any{},
			mode:            schemasOptional,
			expectedSources: 0,
		},
		{
			name:            "not an array",
			args:            map[string]any{schemasParam: "literal-string"},
			mode:            schemasRequired,
			expectedToolErr: true,
		},
		{
			name:            "empty array required",
			args:            map[string]any{schemasParam: []any{}},
			mode:            schemasRequired,
			expectedToolErr: true,
		},
		{
			name:            "empty array optional yields nil",
			args:            map[string]any{schemasParam: []any{}},
			mode:            schemasOptional,
			expectedSources: 0,
		},
		{
			name:            "item not an object",
			args:            map[string]any{schemasParam: []any{"raw-string-instead-of-object"}},
			mode:            schemasRequired,
			expectedToolErr: true,
		},
		{
			name:            "missing sql key",
			args:            map[string]any{schemasParam: []any{map[string]any{"label": "x"}}},
			mode:            schemasRequired,
			expectedToolErr: true,
		},
		{
			name:            "sql wrong type",
			args:            map[string]any{schemasParam: []any{map[string]any{"sql": 42}}},
			mode:            schemasRequired,
			expectedToolErr: true,
		},
		{
			name:            "sql whitespace-only",
			args:            map[string]any{schemasParam: []any{map[string]any{"sql": "   \n\t"}}},
			mode:            schemasRequired,
			expectedToolErr: true,
		},
		{
			name:            "label wrong type",
			args:            map[string]any{schemasParam: []any{map[string]any{"sql": usersDDL, "label": 7}}},
			mode:            schemasRequired,
			expectedToolErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args

			sources, toolErr := extractSchemas(req, tc.mode)

			if tc.expectedToolErr {
				require.NotNil(t, toolErr, "expected tool-level error")
				require.Nil(t, sources)
				return
			}
			require.Nil(t, toolErr)
			require.Len(t, sources, tc.expectedSources)
			if tc.expectedSQL != "" {
				require.Equal(t, tc.expectedSQL, sources[0].SQL)
			}
			if tc.expectedLabel != "" {
				require.Equal(t, tc.expectedLabel, sources[0].Label)
			}
		})
	}
}

func TestListTablesHandler(t *testing.T) {
	tests := []struct {
		name              string
		args              map[string]any
		expectedToolErr   bool
		expectedEnvErrs   bool
		expectedCode      string
		expectedTables    []string
		expectedWarnCount int
	}{
		{
			name:           "two tables across two sources",
			args:           map[string]any{schemasParam: []any{map[string]any{"sql": usersDDL}, map[string]any{"sql": ordersDDL}}},
			expectedTables: []string{"users", "orders"},
		},
		{
			name:              "schema with no CREATE TABLE yields empty list and a warning",
			args:              map[string]any{schemasParam: []any{map[string]any{"sql": "SELECT 1"}}},
			expectedTables:    []string{},
			expectedWarnCount: 1,
		},
		{
			name:            "missing schemas yields tool error",
			args:            map[string]any{},
			expectedToolErr: true,
		},
		{
			name:            "empty schemas array yields tool error",
			args:            map[string]any{schemasParam: []any{}},
			expectedToolErr: true,
		},
		{
			name:            "malformed item yields tool error",
			args:            map[string]any{schemasParam: []any{"not-an-object"}},
			expectedToolErr: true,
		},
		{
			name:            "DDL with parse error surfaces as envelope error",
			args:            map[string]any{schemasParam: []any{map[string]any{"sql": "CREATE TABLEE bad (id INT)"}}},
			expectedEnvErrs: true,
			expectedCode:    "42601",
		},
		{
			name: "duplicate table names produce a schema_warning",
			args: map[string]any{schemasParam: []any{
				map[string]any{"sql": usersDDL},
				map[string]any{"sql": usersDDL},
			}},
			expectedTables:    []string{"users"},
			expectedWarnCount: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := ListTablesHandler(testParserVersion)
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args

			res, err := handler(context.Background(), req)
			require.NoError(t, err)
			require.NotNil(t, res)

			if tc.expectedToolErr {
				require.True(t, res.IsError, "expected tool-level error")
				return
			}

			env := requireEnvelope(t, res)
			require.Equal(t, testParserVersion, env.ParserVersion)
			require.Equal(t, output.TierSchemaFile, env.Tier)
			require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)

			if tc.expectedEnvErrs {
				require.NotEmpty(t, env.Errors)
				require.Nil(t, env.Data)
				require.Equal(t, tc.expectedCode, env.Errors[0].Code)
				return
			}

			var data struct {
				Tables []string `json:"tables"`
			}
			require.NoError(t, json.Unmarshal(env.Data, &data))
			require.Equal(t, tc.expectedTables, data.Tables)

			// Empty slice must marshal as "[]" not "null" so clients
			// don't need to special-case nil. Re-decode the raw JSON to
			// catch a regression here.
			require.NotContains(t, string(env.Data), "null")

			require.Len(t, env.Errors, tc.expectedWarnCount)
			for _, e := range env.Errors {
				require.Equal(t, schemawarn.Code, e.Code)
				require.Equal(t, output.SeverityWarning, e.Severity)
			}
		})
	}
}

func TestDescribeTableHandler(t *testing.T) {
	tests := []struct {
		name                    string
		args                    map[string]any
		expectedToolErr         bool
		expectedTableName       string
		expectedNotFound        bool
		expectedAvailableTables []string
	}{
		{
			name:              "happy path",
			args:              map[string]any{"table": "users", schemasParam: []any{map[string]any{"sql": usersDDL}}},
			expectedTableName: "users",
		},
		{
			name:              "case-insensitive lookup",
			args:              map[string]any{"table": "USERS", schemasParam: []any{map[string]any{"sql": usersDDL}}},
			expectedTableName: "users",
		},
		{
			name: "table not found surfaces 42P01 with available_tables",
			args: map[string]any{"table": "missing", schemasParam: []any{
				map[string]any{"sql": usersDDL},
				map[string]any{"sql": ordersDDL},
			}},
			expectedNotFound:        true,
			expectedAvailableTables: []string{"users", "orders"},
		},
		{
			name:             "table not found in empty catalog omits available_tables",
			args:             map[string]any{"table": "missing", schemasParam: []any{map[string]any{"sql": "SELECT 1"}}},
			expectedNotFound: true,
		},
		{
			name:            "missing table param",
			args:            map[string]any{schemasParam: []any{map[string]any{"sql": usersDDL}}},
			expectedToolErr: true,
		},
		{
			name:            "missing schemas param",
			args:            map[string]any{"table": "users"},
			expectedToolErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := DescribeTableHandler(testParserVersion)
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args

			res, err := handler(context.Background(), req)
			require.NoError(t, err)
			require.NotNil(t, res)

			if tc.expectedToolErr {
				require.True(t, res.IsError, "expected tool-level error")
				return
			}

			env := requireEnvelope(t, res)
			require.Equal(t, testParserVersion, env.ParserVersion)
			require.Equal(t, output.TierSchemaFile, env.Tier)
			require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)

			if tc.expectedNotFound {
				require.Nil(t, env.Data)
				notFound := findErrorByCode(t, env.Errors, undefinedTableCode)
				require.Equal(t, output.SeverityError, notFound.Severity)
				if len(tc.expectedAvailableTables) > 0 {
					// JSON round-trip turns []string into []any; build the
					// expected slice in the same shape rather than asserting
					// against the original []string.
					expected := make([]any, len(tc.expectedAvailableTables))
					for i, name := range tc.expectedAvailableTables {
						expected[i] = name
					}
					require.Equal(t, expected, notFound.Context["available_tables"])
				} else {
					require.Nil(t, notFound.Context)
				}
				return
			}

			require.Empty(t, env.Errors)
			require.NotNil(t, env.Data)
			var tbl catalog.Table
			require.NoError(t, json.Unmarshal(env.Data, &tbl))
			require.Equal(t, tc.expectedTableName, tbl.Name)
			require.NotEmpty(t, tbl.Columns)
		})
	}
}
