// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package conn

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Fixtures live in testdata/explain_ddl/*.txt and were captured from a
// `cockroach demo --no-example-database` cluster running EXPLAIN (DDL,
// SHAPE) against representative DDLs. They are the source of truth for
// the parser's acceptance contract: regenerate with the probe documented
// in the PR if a future CRDB version changes the SHAPE output format.

func TestParseExplainDDLShapeFixtures(t *testing.T) {
	tests := []struct {
		name               string
		fixture            string
		expectedStatement  string
		expectedOperations []DDLOperation
	}{
		{
			name:              "add column without backfill",
			fixture:           "add_column.txt",
			expectedStatement: "ALTER TABLE defaultdb.public.users ADD COLUMN age INT8",
			expectedOperations: []DDLOperation{
				{Op: "execute 4 system table mutations transactions"},
			},
		},
		{
			name:              "add column with not null default triggers backfill",
			fixture:           "add_column_not_null_default.txt",
			expectedStatement: "ALTER TABLE defaultdb.public.users ADD COLUMN status STRING NOT NULL DEFAULT ‹'active'›",
			expectedOperations: []DDLOperation{
				{Op: "execute 2 system table mutations transactions"},
				{
					Op:      "backfill using primary index users_pkey- in relation users",
					Targets: []string{"into users_pkey+ (id; name, status+)"},
				},
				{Op: "execute 2 system table mutations transactions"},
				{
					Op:      "merge temporary indexes into backfilled indexes in relation users",
					Targets: []string{"from users@[5] into users_pkey+"},
				},
				{Op: "execute 1 system table mutations transaction"},
				{Op: "validate UNIQUE constraint backed by index users_pkey+ in relation users"},
				{Op: "validate NOT NULL constraint on column status+ in index users_pkey+ in relation users"},
				{Op: "execute 4 system table mutations transactions"},
			},
		},
		{
			name:              "create index",
			fixture:           "create_index.txt",
			expectedStatement: "CREATE INDEX ON defaultdb.public.users (id, name)",
			expectedOperations: []DDLOperation{
				{Op: "execute 2 system table mutations transactions"},
				{
					Op:      "backfill using primary index users_pkey in relation users",
					Targets: []string{"into users_id_name_idx+ (id, name)"},
				},
				{Op: "execute 2 system table mutations transactions"},
				{
					Op:      "merge temporary indexes into backfilled indexes in relation users",
					Targets: []string{"from users@[5] into users_id_name_idx+"},
				},
				{Op: "execute 1 system table mutations transaction"},
				{Op: "validate UNIQUE constraint backed by index users_id_name_idx+ in relation users"},
				{Op: "execute 2 system table mutations transactions"},
			},
		},
		{
			name:              "drop index cascade",
			fixture:           "drop_index_cascade.txt",
			expectedStatement: "DROP INDEX defaultdb.public.users@idx_users_name CASCADE",
			expectedOperations: []DDLOperation{
				{Op: "execute 4 system table mutations transactions"},
			},
		},
		{
			name:              "alter primary key noop",
			fixture:           "alter_primary_key.txt",
			expectedStatement: "ALTER TABLE defaultdb.public.users ALTER PRIMARY KEY USING COLUMNS (id)",
			expectedOperations: []DDLOperation{
				{Op: "execute 1 system table mutations transaction"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "explain_ddl", tc.fixture))
			require.NoError(t, err, "read fixture")

			statement, operations, err := parseExplainDDLShape(string(data))
			require.NoError(t, err)
			require.Equal(t, tc.expectedStatement, statement)
			require.Equal(t, tc.expectedOperations, operations)
		})
	}
}

func TestParseExplainDDLShapeErrors(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		expectedErr string
	}{
		{
			name:        "empty input",
			input:       "",
			expectedErr: "empty input",
		},
		{
			name:        "missing header prefix",
			input:       "Some other plan;\n └── execute 1 system table mutations transaction\n",
			expectedErr: `first line missing "Schema change plan for " prefix`,
		},
		{
			name:        "unrecognized connector prefix",
			input:       "Schema change plan for FOO;\n\t-> not a tree connector\n",
			expectedErr: "unrecognized tree-connector prefix",
		},
		{
			name: "operation connector with empty content",
			// stripConnector returns ok=false on empty content, so the
			// line falls through to "unrecognized" rather than producing
			// an empty Op. This pins that behavior so an emit of
			// `" └── \n"` from a future CRDB version is rejected loudly,
			// not stored as a blank operation.
			input:       "Schema change plan for FOO;\n └── \n",
			expectedErr: "unrecognized tree-connector prefix",
		},
		{
			name:        "target line before any operation",
			input:       "Schema change plan for FOO;\n │    └── orphan target\n",
			expectedErr: "target before any operation",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parseExplainDDLShape(tc.input)
			require.ErrorContains(t, err, tc.expectedErr)
		})
	}
}

// TestParseExplainDDLShapeStatementTrimming pins how the header line is
// canonicalized. The fixtures all happen to end with exactly one
// trailing semicolon; these cases lock in the behavior at the
// boundaries of that assumption so a future CRDB version that drops or
// doubles the semicolon does not silently shift the Statement field.
func TestParseExplainDDLShapeStatementTrimming(t *testing.T) {
	tests := []struct {
		name              string
		header            string
		expectedStatement string
	}{
		{
			name:              "single trailing semicolon stripped",
			header:            "Schema change plan for ALTER TABLE t ADD COLUMN x INT;",
			expectedStatement: "ALTER TABLE t ADD COLUMN x INT",
		},
		{
			name:              "no trailing semicolon preserved as-is",
			header:            "Schema change plan for ALTER TABLE t ADD COLUMN x INT",
			expectedStatement: "ALTER TABLE t ADD COLUMN x INT",
		},
		{
			name: "double trailing semicolon strips only one",
			// TrimSuffix is one-shot by design — assert that so a
			// future regression to a strip-all-trailing-semicolons
			// implementation is caught here.
			header:            "Schema change plan for ALTER TABLE t ADD COLUMN x INT;;",
			expectedStatement: "ALTER TABLE t ADD COLUMN x INT;",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := tc.header + "\n └── execute 1 system table mutations transaction\n"
			statement, _, err := parseExplainDDLShape(input)
			require.NoError(t, err)
			require.Equal(t, tc.expectedStatement, statement)
		})
	}
}

// TestDDLExplainResultJSONShape locks the JSON contract for
// DDLExplainResult. CLI envelopes and MCP tool results both ship this
// struct as `data`; renaming or dropping the JSON tags would silently
// break agent clients that read fields by name. An assertion on a
// fully-populated value catches both rename and omitempty regressions.
func TestDDLExplainResultJSONShape(t *testing.T) {
	result := DDLExplainResult{
		Statement: "ALTER TABLE t ADD COLUMN x INT",
		Operations: []DDLOperation{
			{Op: "execute 4 system table mutations transactions"},
			{
				Op:      "backfill using primary index t_pkey- in relation t",
				Targets: []string{"into t_pkey+ (id; x+)"},
			},
		},
		RawText: "raw plan text",
	}

	data, err := json.Marshal(result)
	require.NoError(t, err)

	expected := `{
		"statement": "ALTER TABLE t ADD COLUMN x INT",
		"operations": [
			{"op": "execute 4 system table mutations transactions"},
			{
				"op": "backfill using primary index t_pkey- in relation t",
				"targets": ["into t_pkey+ (id; x+)"]
			}
		],
		"raw_text": "raw plan text"
	}`
	require.JSONEq(t, expected, string(data))
}
