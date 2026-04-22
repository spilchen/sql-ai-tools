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

// ExplainResult is the structured form of a default `EXPLAIN <stmt>`.
//
// Header captures the leading `key: value` rows that appear before the
// operator tree (typically distribution and vectorized). Plan is the
// parsed operator forest. RawRows is the original tabular output the
// cluster returned, retained so the CLI text mode can render the plan
// exactly as `cockroach sql` would and so agents can re-parse if they
// need to. ExplainResult is only constructed on the success path; any
// failure (query, scan, parse) returns the zero value plus an error.
type ExplainResult struct {
	Header  map[string]string `json:"header,omitempty"`
	Plan    []PlanNode        `json:"plan"`
	RawRows []string          `json:"raw_rows"`
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

// Explain runs `EXPLAIN <sql>` against the cluster and returns the
// parsed plan tree alongside the raw tabular output.
//
// EXPLAIN (without ANALYZE) does not execute the wrapped statement, so
// no read-only safety wrapper is applied here; the dedicated allowlist
// (issue #21) layers on top. Cluster errors (syntax in the wrapped
// statement, perm denied, etc.) are returned wrapped; callers surface
// them as generic envelope errors today. SQLSTATE-aware enrichment for
// pgwire errors is a future enhancement, not a current contract.
//
// On any query/scan/parse failure after a successful connect, the
// underlying connection is closed and the Manager reverts to its
// pre-connect state, mirroring Ping's recovery contract.
func (m *Manager) Explain(ctx context.Context, sql string) (ExplainResult, error) {
	if err := m.connect(ctx); err != nil {
		return ExplainResult{}, err
	}

	result, err := m.runExplain(ctx, sql)
	if err != nil {
		m.conn.Close(ctx) //nolint:errcheck // best-effort cleanup
		m.conn = nil
		return ExplainResult{}, err
	}
	return result, nil
}

// runExplain is the inner half of Explain that owns the query/scan/parse
// pipeline. Splitting it out lets Explain centralize the connection
// recovery: any error returned here triggers the same close-and-nil
// sequence in the caller, so the three failure modes (Query, Scan,
// rows.Err, parse) cannot drift apart.
func (m *Manager) runExplain(ctx context.Context, sql string) (ExplainResult, error) {
	rows, err := m.conn.Query(ctx, "EXPLAIN "+sql)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("run EXPLAIN: %w", err)
	}
	defer rows.Close()

	var raw []string
	for rows.Next() {
		var info string
		if err := rows.Scan(&info); err != nil {
			return ExplainResult{}, fmt.Errorf("scan EXPLAIN row: %w", err)
		}
		raw = append(raw, info)
	}
	if err := rows.Err(); err != nil {
		return ExplainResult{}, fmt.Errorf("read EXPLAIN rows: %w", err)
	}

	header, plan, err := parseExplainTree(raw)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("parse EXPLAIN output: %w", err)
	}
	return ExplainResult{Header: header, Plan: plan, RawRows: raw}, nil
}

// ExplainDDL runs `EXPLAIN (DDL, SHAPE) <sql>` against the cluster and
// returns the parsed schema-change plan alongside the raw text the
// cluster returned.
//
// EXPLAIN (DDL, SHAPE) does not execute the wrapped DDL — it only asks
// the declarative schema changer to compile a plan — so no read-only
// safety wrapper is applied here; the dedicated allowlist (issue #21)
// will layer on top by intercepting the SQL before it reaches this
// method, leaving runExplainDDL as the single chokepoint to wrap.
//
// On any query/scan/parse failure after a successful connect, the
// underlying connection is closed and the Manager reverts to its
// pre-connect state, mirroring Explain's recovery contract.
func (m *Manager) ExplainDDL(ctx context.Context, sql string) (DDLExplainResult, error) {
	if err := m.connect(ctx); err != nil {
		return DDLExplainResult{}, err
	}

	result, err := m.runExplainDDL(ctx, sql)
	if err != nil {
		m.conn.Close(ctx) //nolint:errcheck // best-effort cleanup
		m.conn = nil
		return DDLExplainResult{}, err
	}
	return result, nil
}

// runExplainDDL is the inner half of ExplainDDL that owns the
// query/scan/parse pipeline. SHAPE output is contractually a single row
// whose `info` column is the entire multi-line plan; we iterate with
// Query (rather than QueryRow) so we can fail loudly on a future CRDB
// version that splits the output across rows, matching the parser's
// "be strict so format changes surface here" discipline. Splitting this
// out lets ExplainDDL centralize the connection-recovery sequence so
// the failure modes (Query, Scan, rows.Err, multi-row, parse) cannot
// drift apart.
func (m *Manager) runExplainDDL(ctx context.Context, sql string) (DDLExplainResult, error) {
	rows, err := m.conn.Query(ctx, "EXPLAIN (DDL, SHAPE) "+sql)
	if err != nil {
		return DDLExplainResult{}, fmt.Errorf("run EXPLAIN (DDL, SHAPE): %w", err)
	}
	defer rows.Close()

	var raw []string
	for rows.Next() {
		var info string
		if err := rows.Scan(&info); err != nil {
			return DDLExplainResult{}, fmt.Errorf("scan EXPLAIN (DDL, SHAPE) row: %w", err)
		}
		raw = append(raw, info)
	}
	if err := rows.Err(); err != nil {
		return DDLExplainResult{}, fmt.Errorf("read EXPLAIN (DDL, SHAPE) rows: %w", err)
	}
	if len(raw) != 1 {
		return DDLExplainResult{}, fmt.Errorf(
			"EXPLAIN (DDL, SHAPE) returned %d rows, expected exactly 1 (CRDB output format may have changed)", len(raw))
	}

	text := raw[0]
	statement, operations, err := parseExplainDDLShape(text)
	if err != nil {
		return DDLExplainResult{}, fmt.Errorf("parse EXPLAIN (DDL, SHAPE) output: %w", err)
	}
	return DDLExplainResult{Statement: statement, Operations: operations, RawText: text}, nil
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
