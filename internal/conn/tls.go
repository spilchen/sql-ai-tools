// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package conn

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// TLSParams holds the four libpq TLS knobs that callers may supply
// through CLI flags or MCP tool inputs separately from the DSN. All
// fields are optional; an empty TLSParams is a no-op when merged.
//
// The field names shadow libpq URI parameters of the same spelling
// (sslmode / sslrootcert / sslcert / sslkey); pgx honors them via the
// standard `?sslmode=...&sslrootcert=...` query syntax, so
// MergeTLSParams' only job is to splice non-empty fields into the
// DSN's query string and detect conflicts with values already present.
type TLSParams struct {
	SSLMode     string
	SSLRootCert string
	SSLCert     string
	SSLKey      string
}

// IsZero reports whether p has no fields set. Provided so callers can
// skip MergeTLSParams' parse step when no TLS knobs were supplied
// without duplicating the field-by-field check.
func (p TLSParams) IsZero() bool {
	return p.SSLMode == "" && p.SSLRootCert == "" && p.SSLCert == "" && p.SSLKey == ""
}

// MergeTLSParams returns dsn with non-empty TLSParams fields applied
// as URI query parameters. Returns dsn unchanged when p is zero.
//
// Pairing policy: SSLCert and SSLKey must be supplied together. A
// half-pair (only one of the two) is a libpq misconfiguration that
// otherwise surfaces as an opaque pgx connect error several frames
// later, so the merge fails up front instead.
//
// Conflict policy: it is an error for the same parameter to be
// supplied via both the DSN's query string and TLSParams when the
// DSN's value is non-empty, so a typo cannot silently win. An empty
// DSN-side value (e.g. `?sslmode=`) is treated as absent.
//
// Form policy: when any TLS field is set, the DSN must be in URI form
// (`postgres://` or `postgresql://`). pgx also accepts the keyword/value
// form ("host=... user=..."), but splicing flag-sourced URI params into
// it would require non-trivial reconstruction; the keyword form is
// rejected with a clear error so the user can switch forms or move the
// TLS knobs into the DSN itself.
func MergeTLSParams(dsn string, p TLSParams) (string, error) {
	if p.IsZero() {
		return dsn, nil
	}
	if (p.SSLCert == "") != (p.SSLKey == "") {
		return "", errors.New("sslcert and sslkey must be supplied together")
	}
	trimmed := strings.TrimSpace(dsn)
	if trimmed == "" {
		return "", errors.New("TLS parameters require a DSN")
	}
	if !strings.HasPrefix(trimmed, "postgres://") && !strings.HasPrefix(trimmed, "postgresql://") {
		return "", errors.New("TLS parameters require a postgres:// URI DSN; the keyword/value form is not supported")
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}
	q := u.Query()

	// The slice (rather than a map) makes the conflict-error order
	// deterministic, which keeps tests stable and the diagnostic
	// reproducible across Go versions.
	fields := []struct {
		name string
		val  string
	}{
		{"sslmode", p.SSLMode},
		{"sslrootcert", p.SSLRootCert},
		{"sslcert", p.SSLCert},
		{"sslkey", p.SSLKey},
	}
	for _, f := range fields {
		if f.val == "" {
			continue
		}
		if existing := q.Get(f.name); existing != "" {
			return "", fmt.Errorf(
				"TLS parameter %q already present in DSN (=%q); remove the flag or the DSN param",
				f.name, existing)
		}
		q.Set(f.name, f.val)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
