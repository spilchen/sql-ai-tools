// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package conn

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTLSParamsIsZero pins the field-by-field zero check that callers
// use to short-circuit MergeTLSParams. A regression that drops one of
// the four fields would let a stray flag silently bypass conflict
// detection on subsequent calls.
func TestTLSParamsIsZero(t *testing.T) {
	tests := []struct {
		name           string
		input          TLSParams
		expectedIsZero bool
	}{
		{name: "fully empty", input: TLSParams{}, expectedIsZero: true},
		{name: "sslmode set", input: TLSParams{SSLMode: "verify-full"}},
		{name: "sslrootcert set", input: TLSParams{SSLRootCert: "/p/ca.crt"}},
		{name: "sslcert set", input: TLSParams{SSLCert: "/p/client.crt"}},
		{name: "sslkey set", input: TLSParams{SSLKey: "/p/client.key"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expectedIsZero, tc.input.IsZero())
		})
	}
}

// TestMergeTLSParamsNoOp verifies the fast path: when no TLS knobs
// are supplied, the DSN flows through verbatim — even pathological
// inputs that the URI parser would reject. Pre-existing TLS params in
// the DSN must be left alone (they belong to the user, not to the
// merger).
func TestMergeTLSParamsNoOp(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "URI form", input: "postgres://root@h:26257/db"},
		{name: "URI with existing TLS params", input: "postgres://root@h:26257/db?sslmode=verify-full&sslrootcert=/p/ca.crt"},
		{name: "keyword/value form", input: "host=h port=26257 user=root sslmode=require"},
		{name: "empty", input: ""},
		{name: "garbage", input: "not a dsn at all"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := MergeTLSParams(tc.input, TLSParams{})
			require.NoError(t, err)
			require.Equal(t, tc.input, out)
		})
	}
}

// TestMergeTLSParamsAppliesFields verifies the round-trip: each
// non-empty TLSParams field appears as a query parameter on the
// returned DSN with the supplied value. Asserting through url.Parse
// (rather than substring matching) guards against a regression that
// double-encodes a value or appends to RawQuery without re-encoding.
func TestMergeTLSParamsAppliesFields(t *testing.T) {
	tests := []struct {
		name           string
		dsn            string
		params         TLSParams
		expectedParams map[string]string
	}{
		{
			name: "all four on bare URI",
			dsn:  "postgres://root@h:26257/db",
			params: TLSParams{
				SSLMode:     "verify-full",
				SSLRootCert: "/p/ca.crt",
				SSLCert:     "/p/client.crt",
				SSLKey:      "/p/client.key",
			},
			expectedParams: map[string]string{
				"sslmode":     "verify-full",
				"sslrootcert": "/p/ca.crt",
				"sslcert":     "/p/client.crt",
				"sslkey":      "/p/client.key",
			},
		},
		{
			name:           "single field only",
			dsn:            "postgres://root@h:26257/db",
			params:         TLSParams{SSLMode: "require"},
			expectedParams: map[string]string{"sslmode": "require"},
		},
		{
			name:   "preserves non-TLS query params",
			dsn:    "postgres://root@h:26257/db?application_name=crdb-sql",
			params: TLSParams{SSLMode: "verify-full"},
			expectedParams: map[string]string{
				"application_name": "crdb-sql",
				"sslmode":          "verify-full",
			},
		},
		{
			name:           "value with spaces is URL-encoded",
			dsn:            "postgres://root@h:26257/db",
			params:         TLSParams{SSLRootCert: "/path with spaces/ca.crt"},
			expectedParams: map[string]string{"sslrootcert": "/path with spaces/ca.crt"},
		},
		{
			name:           "postgresql:// scheme accepted",
			dsn:            "postgresql://root@h:26257/db",
			params:         TLSParams{SSLMode: "require"},
			expectedParams: map[string]string{"sslmode": "require"},
		},
		{
			name:           "DSN-side empty value is overwritten",
			dsn:            "postgres://root@h:26257/db?sslmode=",
			params:         TLSParams{SSLMode: "verify-full"},
			expectedParams: map[string]string{"sslmode": "verify-full"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := MergeTLSParams(tc.dsn, tc.params)
			require.NoError(t, err)

			u, err := url.Parse(out)
			require.NoError(t, err)
			q := u.Query()
			for k, want := range tc.expectedParams {
				require.Equal(t, want, q.Get(k), "param %q", k)
			}
		})
	}
}

// TestMergeTLSParamsConflict pins the fail-loud contract: when both
// the DSN and TLSParams supply a non-empty value for the same key,
// the merge errors rather than silently picking one. The error names
// the parameter so the user can fix it without guesswork.
func TestMergeTLSParamsConflict(t *testing.T) {
	tests := []struct {
		name           string
		dsn            string
		params         TLSParams
		expectedErrSub string
	}{
		{
			name:           "sslmode conflict",
			dsn:            "postgres://h/db?sslmode=require",
			params:         TLSParams{SSLMode: "verify-full"},
			expectedErrSub: `"sslmode"`,
		},
		{
			name:           "sslrootcert conflict reports its own name",
			dsn:            "postgres://h/db?sslrootcert=/a.crt",
			params:         TLSParams{SSLRootCert: "/b.crt"},
			expectedErrSub: `"sslrootcert"`,
		},
		{
			name: "first conflicting field is reported",
			dsn:  "postgres://h/db?sslmode=require&sslcert=/a.crt&sslkey=/a.key",
			params: TLSParams{
				SSLMode: "verify-full",
				SSLCert: "/b.crt",
				SSLKey:  "/b.key",
			},
			// The fields slice in MergeTLSParams orders sslmode before
			// sslcert; pinning that order keeps the diagnostic
			// reproducible across Go versions. SSLKey is supplied
			// alongside SSLCert to satisfy the pairing rule so this
			// case reaches the conflict-detection step.
			expectedErrSub: `"sslmode"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := MergeTLSParams(tc.dsn, tc.params)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.expectedErrSub)
		})
	}
}

// TestMergeTLSParamsRejectsNonURIDSN pins the form policy: keyword/
// value DSNs and empty DSNs are rejected when any TLS knob is
// supplied, because splicing flag-sourced URI params into them would
// be ambiguous. The no-op path (TLSParams{}) keeps these inputs
// flowing through unchanged — that contract is covered separately by
// TestMergeTLSParamsNoOp.
func TestMergeTLSParamsRejectsNonURIDSN(t *testing.T) {
	tests := []struct {
		name           string
		dsn            string
		expectedErrSub string
	}{
		{
			name:           "keyword/value form",
			dsn:            "host=h port=26257 user=root",
			expectedErrSub: "URI DSN",
		},
		{
			name:           "empty dsn",
			dsn:            "",
			expectedErrSub: "require a DSN",
		},
		{
			name:           "whitespace-only dsn",
			dsn:            "   ",
			expectedErrSub: "require a DSN",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := MergeTLSParams(tc.dsn, TLSParams{SSLMode: "verify-full"})
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.expectedErrSub)
		})
	}
}

// TestMergeTLSParamsRejectsHalfCertPair pins the pairing rule:
// SSLCert and SSLKey must travel together. A half-pair is a libpq
// misconfiguration whose pgx error message is unhelpfully far from
// the cause, so the merge rejects it at the boundary.
func TestMergeTLSParamsRejectsHalfCertPair(t *testing.T) {
	tests := []struct {
		name   string
		params TLSParams
	}{
		{name: "sslcert without sslkey", params: TLSParams{SSLCert: "/p/client.crt"}},
		{name: "sslkey without sslcert", params: TLSParams{SSLKey: "/p/client.key"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := MergeTLSParams("postgres://h/db", tc.params)
			require.Error(t, err)
			require.Contains(t, err.Error(), "sslcert and sslkey")
		})
	}
}

// TestMergeTLSParamsMalformedURI exercises the parse-error path. A
// DSN whose scheme passes the prefix gate but whose body is malformed
// must surface a wrapped parser error rather than silently producing
// a half-merged DSN.
func TestMergeTLSParamsMalformedURI(t *testing.T) {
	// A control character in the host triggers url.Parse to fail; the
	// scheme check still passes so the parse step is reached.
	_, err := MergeTLSParams("postgres://h\x7fost/db", TLSParams{SSLMode: "require"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse dsn")
}
