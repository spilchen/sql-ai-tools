// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package semcheck

import (
	"testing"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/catalog"
	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/validateresult"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name                   string
		sql                    string
		withCatalog            bool
		expectedCategories     []string
		expectedTypeCheck      validateresult.CheckStatus
		expectedNameResolution validateresult.CheckStatus
	}{
		{
			name:                   "all clean",
			sql:                    "SELECT 1; SELECT * FROM users",
			withCatalog:            true,
			expectedTypeCheck:      validateresult.CheckOK,
			expectedNameResolution: validateresult.CheckOK,
		},
		{
			name:                   "name resolution skipped without catalog",
			sql:                    "SELECT * FROM usrs",
			expectedTypeCheck:      validateresult.CheckOK,
			expectedNameResolution: validateresult.CheckSkipped,
		},
		{
			name:        "two distinct typos in one envelope",
			sql:         "SELECT * FROM usrs; SELECT nme FROM users",
			withCatalog: true,
			expectedCategories: []string{
				diag.CategoryUnknownTable,
				diag.CategoryUnknownColumn,
			},
			expectedTypeCheck:      validateresult.CheckOK,
			expectedNameResolution: validateresult.CheckFailed,
		},
		{
			name:        "type error in stmt 1 does not hide name error in stmt 2",
			sql:         "SELECT 1 + 'hello'; SELECT * FROM usrs",
			withCatalog: true,
			expectedCategories: []string{
				diag.CategoryTypeMismatch,
				diag.CategoryUnknownTable,
			},
			expectedTypeCheck:      validateresult.CheckFailed,
			expectedNameResolution: validateresult.CheckFailed,
		},
		{
			name:        "unknown table suppresses cascaded column error",
			sql:         "SELECT nme FROM nosuch; SELECT 1",
			withCatalog: true,
			expectedCategories: []string{
				diag.CategoryUnknownTable,
			},
			expectedTypeCheck:      validateresult.CheckOK,
			expectedNameResolution: validateresult.CheckFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stmts, err := parser.Parse(tc.sql)
			require.NoError(t, err)

			var cat *catalog.Catalog
			if tc.withCatalog {
				cat = usersOnlyCatalog(t)
			}

			res, errs := Run(stmts, tc.sql, cat)

			require.Equal(t, tc.expectedTypeCheck, res.TypeCheck)
			require.Equal(t, tc.expectedNameResolution, res.NameResolution)
			require.Len(t, errs, len(tc.expectedCategories))
			for i, want := range tc.expectedCategories {
				// Type-check errors carry only the SQLSTATE code
				// (diag.FromTypeError does not populate Category),
				// while name-check errors set Category directly.
				// CategoryForCode normalises both into the same
				// taxonomy so the test reads one way.
				got := errs[i].Category
				if got == "" {
					got = diag.CategoryForCode(errs[i].Code)
				}
				require.Equal(t, want, got,
					"error %d: code=%q message=%q",
					i, errs[i].Code, errs[i].Message)
			}
		})
	}
}

// TestRunPhaseOrdering pins the phase order so consumers (and the
// validate command's text renderer) can rely on it. Type-check errors
// always come before table-name errors, which always come before
// column-name errors.
func TestRunPhaseOrdering(t *testing.T) {
	const sql = "SELECT 1 + 'x'; SELECT * FROM usrs; SELECT u.nme FROM users u"
	stmts, err := parser.Parse(sql)
	require.NoError(t, err)

	res, errs := Run(stmts, sql, usersOnlyCatalog(t))

	require.Equal(t, validateresult.CheckFailed, res.TypeCheck)
	require.Equal(t, validateresult.CheckFailed, res.NameResolution)
	require.Len(t, errs, 3)

	require.Equal(t, diag.CategoryTypeMismatch, diag.CategoryForCode(errs[0].Code))
	require.Equal(t, diag.CategoryUnknownTable, errs[1].Category)
	require.Equal(t, diag.CategoryUnknownColumn, errs[2].Category)
}
