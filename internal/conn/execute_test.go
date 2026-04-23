// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package conn

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/safety"
)

// TestTxOptionsForMode pins the mode-to-txOptions mapping. The
// helper is the iteration-2 fix for the unknown-mode privilege-
// escalation hazard: a future caller bypassing safety.ParseMode must
// fail loudly here rather than silently land in a read-write txn.
func TestTxOptionsForMode(t *testing.T) {
	tests := []struct {
		name              string
		mode              safety.Mode
		expectedAccess    pgx.TxAccessMode
		expectedErrSubstr string
	}{
		{
			name:           "read_only uses pgx.ReadOnly",
			mode:           safety.ModeReadOnly,
			expectedAccess: pgx.ReadOnly,
		},
		{
			// pgx.TxOptions{} leaves AccessMode as the zero value
			// ("", not pgx.ReadWrite). The cluster treats that as
			// the default — a read-write txn — so the helper
			// intentionally does not set AccessMode for the write
			// modes.
			name:           "safe_write leaves AccessMode unset (cluster default = read-write)",
			mode:           safety.ModeSafeWrite,
			expectedAccess: pgx.TxAccessMode(""),
		},
		{
			name:           "full_access leaves AccessMode unset (cluster default = read-write)",
			mode:           safety.ModeFullAccess,
			expectedAccess: pgx.TxAccessMode(""),
		},
		{
			name:              "unknown mode rejected",
			mode:              safety.Mode("garbage"),
			expectedErrSubstr: `unknown safety mode "garbage"`,
		},
		{
			name:              "empty mode rejected (zero value of safety.Mode)",
			mode:              safety.Mode(""),
			expectedErrSubstr: `unknown safety mode ""`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := txOptionsForMode(tc.mode)
			if tc.expectedErrSubstr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.expectedErrSubstr)
				require.Equal(t, pgx.TxOptions{}, opts,
					"error path must return zero-value opts so callers can't accidentally use them")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expectedAccess, opts.AccessMode)
		})
	}
}
