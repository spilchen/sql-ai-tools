// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package versionroute

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseFromTarget(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		expectedQ   Quarter
		expectedOK  bool
		description string
	}{
		{name: "year quarter patch", input: "25.4.0", expectedQ: Quarter{Year: 25, Q: 4}, expectedOK: true},
		{name: "year quarter only", input: "25.4", expectedQ: Quarter{Year: 25, Q: 4}, expectedOK: true},
		{name: "leading v", input: "v26.1.5", expectedQ: Quarter{Year: 26, Q: 1}, expectedOK: true},
		{name: "leading whitespace", input: "  25.2.0  ", expectedQ: Quarter{Year: 25, Q: 2}, expectedOK: true},
		{name: "single component", input: "25", expectedOK: false, description: "MAJOR alone is not a quarter"},
		{name: "non-numeric major", input: "v.4.0", expectedOK: false},
		{name: "non-numeric minor", input: "25.x.0", expectedOK: false},
		{name: "quarter zero", input: "25.0.0", expectedOK: false, description: "Q must be 1..4"},
		{name: "quarter five", input: "25.5.0", expectedOK: false, description: "Q must be 1..4"},
		{name: "negative year", input: "-1.4.0", expectedOK: false, description: "Atoi rejects after our >0 check"},
		{name: "empty", input: "", expectedOK: false},
		{name: "just a v", input: "v", expectedOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, ok := ParseFromTarget(tt.input)
			require.Equal(t, tt.expectedOK, ok, tt.description)
			if tt.expectedOK {
				require.Equal(t, tt.expectedQ, q)
			}
		})
	}
}

func TestParseTag(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		expectedQ  Quarter
		expectedOK bool
	}{
		{name: "v254", input: "v254", expectedQ: Quarter{Year: 25, Q: 4}, expectedOK: true},
		{name: "no v prefix", input: "254", expectedQ: Quarter{Year: 25, Q: 4}, expectedOK: true},
		{name: "v261", input: "v261", expectedQ: Quarter{Year: 26, Q: 1}, expectedOK: true},
		{name: "single year digit", input: "v94", expectedQ: Quarter{Year: 9, Q: 4}, expectedOK: true},
		{name: "quarter zero", input: "v250", expectedOK: false},
		{name: "quarter five", input: "v255", expectedOK: false},
		{name: "too short", input: "v2", expectedOK: false},
		{name: "non-numeric", input: "vXX1", expectedOK: false},
		{name: "empty", input: "", expectedOK: false},
		{name: "just v", input: "v", expectedOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, ok := ParseTag(tt.input)
			require.Equal(t, tt.expectedOK, ok)
			if tt.expectedOK {
				require.Equal(t, tt.expectedQ, q)
			}
		})
	}
}

func TestQuarterFormatters(t *testing.T) {
	q := Quarter{Year: 25, Q: 4}
	require.Equal(t, "25.4", q.String())
	require.Equal(t, "v254", q.Tag())
	require.Equal(t, "crdb-sql-v254", q.BackendName())
	require.False(t, q.IsZero())

	// Zero value has dedicated formatter outputs so callers do not
	// need to gate on IsZero.
	zero := Quarter{}
	require.True(t, zero.IsZero())
	require.Equal(t, "unknown", zero.String())
	require.Equal(t, "", zero.Tag())
	require.Equal(t, "crdb-sql", zero.BackendName())
}

func TestMakeQuarter(t *testing.T) {
	tests := []struct {
		name        string
		year        int
		q           int
		expectedQ   Quarter
		expectedErr string
	}{
		{name: "valid", year: 25, q: 4, expectedQ: Quarter{Year: 25, Q: 4}},
		{name: "year zero rejected", year: 0, q: 4, expectedErr: ">= 1"},
		{name: "negative year rejected", year: -1, q: 2, expectedErr: ">= 1"},
		{name: "quarter zero rejected", year: 25, q: 0, expectedErr: "1..4"},
		{name: "quarter five rejected", year: 25, q: 5, expectedErr: "1..4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MakeQuarter(tt.year, tt.q)
			if tt.expectedErr != "" {
				require.ErrorContains(t, err, tt.expectedErr)
				require.Equal(t, Quarter{}, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.expectedQ, got)
		})
	}
}

func TestStampDiagnostic(t *testing.T) {
	tests := []struct {
		name             string
		stamp            string
		expectedContains string
	}{
		{name: "absent", stamp: "", expectedContains: ""},
		{name: "valid", stamp: "v262", expectedContains: ""},
		{name: "malformed garbage", stamp: "banana", expectedContains: "banana"},
		{name: "malformed quarter zero", stamp: "v250", expectedContains: "v250"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := builtQuarterStamp
			builtQuarterStamp = tt.stamp
			defer func() { builtQuarterStamp = prev }()

			diag := StampDiagnostic()
			if tt.expectedContains == "" {
				require.Empty(t, diag)
				return
			}
			require.NotEmpty(t, diag)
			require.Contains(t, diag, tt.expectedContains)
			require.Contains(t, diag, "Reinstall")
		})
	}
}

func TestParseForkVersion(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		expectedQ  Quarter
		expectedOK bool
	}{
		{name: "standard fork tag", input: "v0.26.2", expectedQ: Quarter{Year: 26, Q: 2}, expectedOK: true},
		{name: "no v prefix", input: "0.26.2", expectedQ: Quarter{Year: 26, Q: 2}, expectedOK: true},
		{name: "with patch suffix", input: "v0.25.4.3", expectedQ: Quarter{Year: 25, Q: 4}, expectedOK: true},
		{name: "with rc suffix", input: "v0.26.2-rc.1", expectedQ: Quarter{Year: 26, Q: 2}, expectedOK: true},
		{name: "too few components", input: "v0.26", expectedOK: false},
		{name: "non-numeric year", input: "v0.xx.2", expectedOK: false},
		{name: "quarter out of range", input: "v0.25.7", expectedOK: false},
		{name: "empty", input: "", expectedOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, ok := parseForkVersion(tt.input)
			require.Equal(t, tt.expectedOK, ok)
			if tt.expectedOK {
				require.Equal(t, tt.expectedQ, q)
			}
		})
	}
}

func TestExtractTargetVersion(t *testing.T) {
	tests := []struct {
		name            string
		args            []string
		expectedRaw     string
		expectedHasFlag bool
	}{
		{
			name:            "space-separated",
			args:            []string{"crdb-sql", "validate", "--target-version", "25.4.0", "-e", "SELECT 1"},
			expectedRaw:     "25.4.0",
			expectedHasFlag: true,
		},
		{
			name:            "equals-separated",
			args:            []string{"crdb-sql", "--target-version=25.4.0", "validate"},
			expectedRaw:     "25.4.0",
			expectedHasFlag: true,
		},
		{
			name:            "absent",
			args:            []string{"crdb-sql", "validate", "-e", "SELECT 1"},
			expectedRaw:     "",
			expectedHasFlag: false,
		},
		{
			name:            "flag present, no value",
			args:            []string{"crdb-sql", "--target-version"},
			expectedRaw:     "",
			expectedHasFlag: true,
		},
		{
			name:            "flag at end with equals empty",
			args:            []string{"crdb-sql", "validate", "--target-version="},
			expectedRaw:     "",
			expectedHasFlag: true,
		},
		{
			name:            "ignored after end-of-options marker",
			args:            []string{"crdb-sql", "validate", "-e", "SELECT 1", "--", "--target-version=25.1.0"},
			expectedRaw:     "",
			expectedHasFlag: false,
		},
		{
			name:            "argv[0] is skipped even if it looks like the flag",
			args:            []string{"--target-version=25.1.0"},
			expectedRaw:     "",
			expectedHasFlag: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, has := extractTargetVersion(tt.args)
			require.Equal(t, tt.expectedHasFlag, has)
			require.Equal(t, tt.expectedRaw, raw)
		})
	}
}
