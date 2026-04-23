// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package safety_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/safety"
)

func TestParseMode(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		expectedMode safety.Mode
		expectedErr  string
	}{
		{name: "empty defaults to read_only", input: "", expectedMode: safety.DefaultMode},
		{name: "read_only", input: "read_only", expectedMode: safety.ModeReadOnly},
		{name: "safe_write", input: "safe_write", expectedMode: safety.ModeSafeWrite},
		{name: "full_access", input: "full_access", expectedMode: safety.ModeFullAccess},
		{name: "unknown rejected", input: "yolo", expectedErr: "invalid safety mode"},
		{name: "case-sensitive", input: "Read_Only", expectedErr: "invalid safety mode"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := safety.ParseMode(tc.input)
			if tc.expectedErr != "" {
				require.ErrorContains(t, err, tc.expectedErr)
				require.Contains(t, err.Error(), "read_only",
					"error message should list valid choices")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expectedMode, m)
		})
	}
}
