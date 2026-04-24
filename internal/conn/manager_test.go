// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package conn

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestNewManagerDoesNotConnect verifies that construction stores the
// DSN without initiating a TCP connection.
func TestNewManagerDoesNotConnect(t *testing.T) {
	mgr := NewManager("postgres://localhost:26257/defaultdb")
	require.Nil(t, mgr.conn, "NewManager must not connect eagerly")
}

// TestWithStatementTimeoutFallback pins the load-bearing safety
// behaviour of WithStatementTimeout: a non-positive duration must
// fall back to DefaultStatementTimeout, never propagate as
// `SET LOCAL statement_timeout = '0ms'` (which CRDB interprets as
// "no timeout" and would silently disable the guardrail). A regression
// that flips the comparison from `<= 0` to `< 0` would let
// WithStatementTimeout(0) leak through and is exactly the bug this
// test exists to catch.
func TestWithStatementTimeoutFallback(t *testing.T) {
	tests := []struct {
		name  string
		input time.Duration
	}{
		{name: "zero falls back to default", input: 0},
		{name: "negative falls back to default", input: -1},
		{name: "large negative falls back to default", input: -1 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewManager("postgres://localhost:26257/defaultdb",
				WithStatementTimeout(tc.input))
			require.Equal(t, DefaultStatementTimeout, mgr.stmtTimeout,
				"non-positive duration must fall back to DefaultStatementTimeout")
		})
	}
}

// TestWithStatementTimeoutHonorsPositive verifies the happy path:
// positive durations are stored verbatim. Pairs with the fallback
// test so the boundary at zero is unambiguously exercised on both
// sides.
func TestWithStatementTimeoutHonorsPositive(t *testing.T) {
	mgr := NewManager("postgres://localhost:26257/defaultdb",
		WithStatementTimeout(5*time.Second))
	require.Equal(t, 5*time.Second, mgr.stmtTimeout)
}

// TestNewManagerDefaultStatementTimeout pins the no-options default
// so a future refactor that drops the field initialisation in
// NewManager cannot silently regress to a zero timeout.
func TestNewManagerDefaultStatementTimeout(t *testing.T) {
	mgr := NewManager("postgres://localhost:26257/defaultdb")
	require.Equal(t, DefaultStatementTimeout, mgr.stmtTimeout)
}

// TestCloseWhenNotConnected verifies that Close is a safe no-op when
// no connection was ever established.
func TestCloseWhenNotConnected(t *testing.T) {
	mgr := NewManager("postgres://localhost:26257/defaultdb")
	require.NoError(t, mgr.Close(context.Background()))
}

// TestPingEmptyDSN verifies that Ping with an empty DSN returns a
// connection error rather than panicking.
func TestPingEmptyDSN(t *testing.T) {
	mgr := NewManager("")
	_, err := mgr.Ping(context.Background())
	require.Error(t, err)
	require.ErrorContains(t, err, "connect to CockroachDB")
}

// TestSanitizeConnErr verifies that credential patterns in connection
// errors are redacted before reaching callers.
func TestSanitizeConnErr(t *testing.T) {
	tests := []struct {
		name      string
		inputErr  error
		forbidden string
		expected  string
	}{
		{
			name:      "strips user:password from postgres URI",
			inputErr:  fmt.Errorf("dial tcp postgres://admin:s3cret@host:26257/db: connection refused"),
			forbidden: "s3cret",
			expected:  "dial tcp postgres://REDACTED@host:26257/db: connection refused",
		},
		{
			name:      "strips user-only (no password) from postgres URI",
			inputErr:  fmt.Errorf("dial tcp postgres://root@host:26257/db: connection refused"),
			forbidden: "",
			expected:  "dial tcp postgres://REDACTED@host:26257/db: connection refused",
		},
		{
			name:      "strips password containing literal @ character",
			inputErr:  fmt.Errorf("dial tcp postgres://admin:p@ss@host:26257/db: connection refused"),
			forbidden: "p@ss",
			expected:  "dial tcp postgres://REDACTED@host:26257/db: connection refused",
		},
		{
			name:      "strips credentials from postgresql:// scheme",
			inputErr:  fmt.Errorf("dial tcp postgresql://user:secret@cloud-host:26257/db: timeout"),
			forbidden: "secret",
			expected:  "dial tcp postgresql://REDACTED@cloud-host:26257/db: timeout",
		},
		{
			name:     "passes through error without URI unchanged",
			inputErr: fmt.Errorf("connection refused"),
			expected: "connection refused",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeConnErr(tc.inputErr)
			require.Equal(t, tc.expected, got.Error())
			if tc.forbidden != "" {
				require.NotContains(t, got.Error(), tc.forbidden)
			}
		})
	}
}

// TestSanitizeConnErrPreservesUnwrap verifies that sanitized errors
// preserve the original error chain for errors.Is / errors.As.
func TestSanitizeConnErrPreservesUnwrap(t *testing.T) {
	sentinel := errors.New("connection refused")
	wrapped := fmt.Errorf("postgres://admin:secret@host:26257/db: %w", sentinel)

	got := sanitizeConnErr(wrapped)
	require.NotContains(t, got.Error(), "secret")
	require.ErrorIs(t, got, sentinel,
		"sanitized error must preserve the original chain for errors.Is")
}

// TestSanitizeConnErrPassthroughPreservesIdentity verifies that errors
// without credentials are returned as-is (no wrapping overhead).
func TestSanitizeConnErrPassthroughPreservesIdentity(t *testing.T) {
	original := fmt.Errorf("connection refused")
	got := sanitizeConnErr(original)
	require.Same(t, original, got,
		"errors without credentials should be returned unchanged")
}

// TestExplainAnyParseTimeRejections covers the dispatcher's pre-cluster
// validation: malformed input, empty input, multi-statement input, and
// statement classes the dispatcher has no EXPLAIN route for. None of
// these reach the cluster, so the test runs without a live DSN.
func TestExplainAnyParseTimeRejections(t *testing.T) {
	tests := []struct {
		name           string
		sql            string
		expectedErrMsg string
	}{
		{
			name:           "syntax error",
			sql:            "SELEKT 1",
			expectedErrMsg: "parse explain input",
		},
		{
			name:           "empty input",
			sql:            "",
			expectedErrMsg: "no statements parsed",
		},
		{
			name:           "multi-statement rejected",
			sql:            "SELECT 1; SELECT 2",
			expectedErrMsg: "explain accepts a single statement",
		},
		{
			name:           "tcl has no route",
			sql:            "BEGIN",
			expectedErrMsg: "no route for statement type",
		},
		{
			name:           "dcl has no route",
			sql:            "GRANT SELECT ON t TO bob",
			expectedErrMsg: "no route for statement type",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewManager("postgres://localhost:26257/defaultdb")
			_, err := mgr.ExplainAny(context.Background(), tc.sql)
			require.Error(t, err)
			require.ErrorContains(t, err, tc.expectedErrMsg)
			require.Nil(t, mgr.conn,
				"parse-time rejections must not open a connection")
		})
	}
}
