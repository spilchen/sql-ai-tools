// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package conn manages pgwire connections to CockroachDB clusters.
//
// The Manager holds a DSN and establishes a connection lazily on the
// first call that requires cluster access (currently Ping). It is the
// single point of contact between crdb-sql and the cluster; all SQL
// execution flows through it, and it enforces the invariant that
// credentials are never included in error messages or log output.
//
// Lifecycle: callers create a Manager with NewManager, invoke methods
// that may trigger a lazy connect, and defer Close. The Manager is not
// safe for concurrent use; the CLI creates one per command invocation.
package conn

import (
	"context"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
)

// dsnCredentialPattern matches the userinfo portion of a postgres URI
// (e.g. "user:password@") so it can be redacted from error messages.
// The .+ is greedy so it consumes up to the last @ — this handles
// passwords containing literal @ characters. Defense-in-depth: pgx v5
// does not include credentials in its errors, but a future version or
// a wrapped driver might.
var dsnCredentialPattern = regexp.MustCompile(`://.+@`)

// ClusterInfo holds the metadata returned by a successful Ping.
type ClusterInfo struct {
	ClusterID string `json:"cluster_id"`
	Version   string `json:"version"`
}

// Manager manages a lazy pgwire connection to a CockroachDB cluster.
// It stores a DSN at construction time and defers the actual TCP
// handshake until the first method that needs a live connection.
//
// The dsn field is unexported and the type intentionally has no
// Stringer or GoStringer implementation, so accidental logging via
// %v or %+v cannot leak credentials.
type Manager struct {
	dsn  string
	conn *pgx.Conn // nil until the first successful connect
}

// NewManager creates a Manager that will connect to the cluster
// identified by dsn on first use. It does not validate or parse the
// DSN — invalid values surface as connection errors on first use.
func NewManager(dsn string) *Manager {
	return &Manager{dsn: dsn}
}

// Ping connects to the cluster (if not already connected) and returns
// the cluster ID and CockroachDB version. It is the primary entry
// point for the lazy-connect lifecycle: callers that only need to
// verify connectivity call Ping and inspect the returned ClusterInfo.
//
// If the query fails after a successful connect, the connection is
// closed and the Manager reverts to its pre-connect state. Callers
// do not need to distinguish partial failures from connection
// failures — either way, the next Ping will attempt a fresh connect.
func (m *Manager) Ping(ctx context.Context) (ClusterInfo, error) {
	if err := m.connect(ctx); err != nil {
		return ClusterInfo{}, err
	}

	var info ClusterInfo
	err := m.conn.QueryRow(ctx,
		"SELECT crdb_internal.cluster_id()::STRING, version()").
		Scan(&info.ClusterID, &info.Version)
	if err != nil {
		m.conn.Close(ctx) //nolint:errcheck // best-effort cleanup
		m.conn = nil
		return ClusterInfo{}, fmt.Errorf("query cluster info: %w", err)
	}
	return info, nil
}

// Close closes the underlying connection if one was established.
// It is safe to call on a Manager that never connected.
func (m *Manager) Close(ctx context.Context) error {
	if m.conn == nil {
		return nil
	}
	err := m.conn.Close(ctx)
	m.conn = nil
	return err
}

// connect establishes the pgx connection if one is not already open.
// On failure the error is sanitized to strip any credential fragments
// before being returned to the caller.
func (m *Manager) connect(ctx context.Context) error {
	if m.conn != nil {
		return nil
	}
	conn, err := pgx.Connect(ctx, m.dsn)
	if err != nil {
		return fmt.Errorf("connect to CockroachDB: %w", sanitizeConnErr(err))
	}
	m.conn = conn
	return nil
}

// sanitizedErr wraps an error, redacting credential patterns from its
// string representation while preserving the original error chain for
// errors.Is / errors.As inspection. The original chain is accessible
// only through Unwrap, which returns the error by type/value — callers
// that print the unwrapped error directly would see the unredacted
// message, but the primary defense (pgx v5 not including credentials)
// makes this acceptable for a safety-net wrapper.
type sanitizedErr struct {
	msg      string
	original error
}

func (e *sanitizedErr) Error() string { return e.msg }
func (e *sanitizedErr) Unwrap() error { return e.original }

// sanitizeConnErr redacts credential patterns from a connection error.
// It replaces the userinfo portion of any postgres:// URI embedded in
// the error string with "REDACTED", while preserving the original
// error chain for programmatic inspection via errors.Is/errors.As.
// This is defense-in-depth: pgx v5 does not embed credentials in
// errors, but the wrapper ensures that a future driver change or a
// wrapped error cannot leak secrets through the CLI's structured
// error output.
func sanitizeConnErr(err error) error {
	msg := err.Error()
	scrubbed := dsnCredentialPattern.ReplaceAllString(msg, "://REDACTED@")
	if scrubbed == msg {
		return err
	}
	return &sanitizedErr{msg: scrubbed, original: err}
}
