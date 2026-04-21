// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package diag

// Category constants match the design doc (search "Error categorization")
// and are the wire-format strings agents receive in the JSON envelope.
const (
	CategorySyntaxError        = "syntax_error"
	CategoryTypeMismatch       = "type_mismatch"
	CategoryUnknownColumn      = "unknown_column"
	CategoryUnknownTable       = "unknown_table"
	CategoryUnknownFunction    = "unknown_function"
	CategoryAmbiguousReference = "ambiguous_reference"
)

// codeCategory maps exact 5-character SQLSTATE codes to categories.
// Entries here override the class-level fallback in classCategory.
var codeCategory = map[string]string{
	"42601": CategorySyntaxError,        // syntax_error
	"42703": CategoryUnknownColumn,      // undefined_column
	"42P01": CategoryUnknownTable,       // undefined_table
	"42883": CategoryUnknownFunction,    // undefined_function
	"42804": CategoryTypeMismatch,       // datatype_mismatch
	"42702": CategoryAmbiguousReference, // ambiguous_column
	"42725": CategoryAmbiguousReference, // ambiguous_function
	"42P08": CategoryAmbiguousReference, // ambiguous_parameter
	"42P09": CategoryAmbiguousReference, // ambiguous_alias
	"42P10": CategoryUnknownColumn,      // invalid_column_reference
}

// classCategory maps the 2-character SQLSTATE class prefix to a
// fallback category used when the exact code is not in codeCategory.
// Class 42 ("Syntax Error or Access Rule Violation") includes
// access-rule codes like 42501 (insufficient_privilege), but this
// tool only sees parser-generated errors where the syntax_error
// fallback is appropriate.
var classCategory = map[string]string{
	"42": CategorySyntaxError,  // Syntax Error or Access Rule Violation
	"22": CategoryTypeMismatch, // Data Exception
}

// CategoryForCode returns the agent-facing category string for the
// given SQLSTATE code. It tries an exact 5-character match first,
// then falls back to a 2-character class-level match. If neither
// matches, it returns the empty string (the Category field in
// output.Error is omitempty, so unmapped codes simply omit the field
// from JSON output).
func CategoryForCode(code string) string {
	if cat, ok := codeCategory[code]; ok {
		return cat
	}
	if len(code) >= 2 {
		if cat, ok := classCategory[code[:2]]; ok {
			return cat
		}
	}
	return ""
}
