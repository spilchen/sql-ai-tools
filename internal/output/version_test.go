// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package output

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateTargetVersion(t *testing.T) {
	tests := []struct {
		name              string
		input             string
		expectedCanonical string
		expectedErr       string
	}{
		{name: "major.minor.patch", input: "25.4.0", expectedCanonical: "25.4.0"},
		{name: "major.minor", input: "25.4", expectedCanonical: "25.4"},
		{name: "leading v stripped", input: "v25.4.0", expectedCanonical: "25.4.0"},
		{name: "leading v with major.minor", input: "v25.4", expectedCanonical: "25.4"},
		{name: "multi-digit components", input: "v123.456.789", expectedCanonical: "123.456.789"},
		{name: "empty rejected", input: "", expectedErr: "must not be empty"},
		{name: "single component rejected", input: "25", expectedErr: "MAJOR.MINOR"},
		{name: "four components rejected", input: "25.4.0.1", expectedErr: "MAJOR.MINOR"},
		{name: "non-numeric rejected", input: "25.x.0", expectedErr: "non-numeric"},
		{name: "trailing dot rejected", input: "25.4.", expectedErr: "empty component"},
		{name: "leading dot rejected", input: ".25.4", expectedErr: "empty component"},
		{name: "negative component rejected", input: "-1.4.0", expectedErr: "non-numeric"},
		{name: "signed component rejected", input: "25.+4.0", expectedErr: "non-numeric"},
		{name: "leading whitespace trimmed", input: "  25.4.0", expectedCanonical: "25.4.0"},
		{name: "trailing whitespace trimmed", input: "25.4.0\n", expectedCanonical: "25.4.0"},
		{name: "whitespace-only rejected", input: "   ", expectedErr: "must not be empty"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateTargetVersion(tc.input)
			if tc.expectedErr != "" {
				require.ErrorContains(t, err, tc.expectedErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expectedCanonical, got)
		})
	}
}

func TestVersionMismatchWarning(t *testing.T) {
	tests := []struct {
		name            string
		parser          string
		target          string
		expectWarning   bool
		expectedMessage string
	}{
		{
			name:            "different major.minor produces warning",
			parser:          "v26.2.0",
			target:          "25.4.0",
			expectWarning:   true,
			expectedMessage: "parser is v26.2 but target is v25.4 — results may differ",
		},
		{
			name:          "matching major.minor produces no warning",
			parser:        "v25.4.0",
			target:        "25.4.1",
			expectWarning: false,
		},
		{
			name:          "matching major.minor without patch",
			parser:        "v25.4.0",
			target:        "25.4",
			expectWarning: false,
		},
		{
			name:          "different major produces warning",
			parser:        "v26.0.0",
			target:        "25.0.0",
			expectWarning: true,
		},
		{
			name:          "different minor produces warning",
			parser:        "v25.3.0",
			target:        "25.4.0",
			expectWarning: true,
		},
		{
			name:          "unknown parser version skips warning",
			parser:        "unknown",
			target:        "25.4.0",
			expectWarning: false,
		},
		{
			name:          "unparseable target skips warning",
			parser:        "v25.4.0",
			target:        "garbage",
			expectWarning: false,
		},
		{
			// Pins that majorMinor uses ParseUint, not Atoi. A
			// regression that swapped back to Atoi would silently
			// start emitting warnings against signed parser
			// versions in dev builds.
			name:          "signed parser component skips warning",
			parser:        "-1.4.0",
			target:        "25.4.0",
			expectWarning: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			warning, ok := VersionMismatchWarning(tc.parser, tc.target)
			require.Equal(t, tc.expectWarning, ok)
			if !tc.expectWarning {
				return
			}
			require.Equal(t, "target_version_mismatch", warning.Code)
			require.Equal(t, SeverityWarning, warning.Severity)
			if tc.expectedMessage != "" {
				require.Equal(t, tc.expectedMessage, warning.Message)
			}
		})
	}
}
