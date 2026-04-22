// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package diag

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCategoryForCode(t *testing.T) {
	tests := []struct {
		name             string
		code             string
		expectedCategory string
	}{
		// Exact code matches.
		{name: "syntax error", code: "42601", expectedCategory: "syntax_error"},
		{name: "undefined column", code: "42703", expectedCategory: "unknown_column"},
		{name: "undefined table", code: "42P01", expectedCategory: "unknown_table"},
		{name: "undefined function", code: "42883", expectedCategory: "unknown_function"},
		{name: "datatype mismatch", code: "42804", expectedCategory: "type_mismatch"},
		{name: "ambiguous column", code: "42702", expectedCategory: "ambiguous_reference"},
		{name: "ambiguous function", code: "42725", expectedCategory: "ambiguous_reference"},
		{name: "ambiguous parameter", code: "42P08", expectedCategory: "ambiguous_reference"},
		{name: "ambiguous alias", code: "42P09", expectedCategory: "ambiguous_reference"},
		{name: "invalid column reference", code: "42P10", expectedCategory: "unknown_column"},
		{name: "insufficient privilege", code: "42501", expectedCategory: "permission_denied"},
		{name: "query canceled", code: "57014", expectedCategory: "query_canceled"},

		// Class-level fallback.
		{name: "unmapped class-42 falls back to syntax_error", code: "42999", expectedCategory: "syntax_error"},
		{name: "data exception class", code: "22012", expectedCategory: "type_mismatch"},
		{name: "connection class fallback", code: "08000", expectedCategory: "connection_error"},
		{name: "connection failure subcode", code: "08006", expectedCategory: "connection_error"},

		// Unknown codes return empty.
		{name: "unknown class returns empty", code: "99999", expectedCategory: ""},
		{name: "empty code returns empty", code: "", expectedCategory: ""},
		{name: "short code returns empty", code: "4", expectedCategory: ""},

		// Non-SQLSTATE code from describe command.
		{name: "schema_warning not mapped", code: "schema_warning", expectedCategory: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CategoryForCode(tc.code)
			require.Equal(t, tc.expectedCategory, got)
		})
	}
}

// TestCategoryForCode_exactOverridesClass verifies that an exact code
// entry takes precedence over the class-level fallback.
func TestCategoryForCode_exactOverridesClass(t *testing.T) {
	got := CategoryForCode("42703")
	require.Equal(t, "unknown_column", got)
}
