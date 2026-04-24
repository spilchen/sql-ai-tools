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
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/tree"
	"github.com/jackc/pgx/v5"
)

// DefaultStatementTimeout is the per-call statement_timeout applied
// inside the transaction wrapper used by Explain, ExplainDDL, and
// Execute when the caller does not override it via
// WithStatementTimeout. 30s is generous enough for EXPLAIN and
// EXPLAIN (DDL, SHAPE) on large schemas, and for typical interactive
// queries via exec, while still preventing a runaway statement from
// hanging an agent indefinitely.
const DefaultStatementTimeout = 30 * time.Second

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
//
// The stmtTimeout field is the SET LOCAL statement_timeout applied
// inside the transaction wrapper used by every Explain / ExplainDDL
// / Execute call. It is set once at construction (default
// DefaultStatementTimeout) via WithStatementTimeout; the Manager is
// not safe for concurrent use, so the field never needs synchronisation.
type Manager struct {
	dsn         string
	stmtTimeout time.Duration
	conn        *pgx.Conn // nil until the first successful connect
}

// Option configures a Manager at construction time. Implemented via
// the functional-options pattern so future knobs (e.g. application_name,
// retry budget) extend the API without breaking call sites.
type Option func(*Manager)

// WithStatementTimeout overrides the per-call statement_timeout
// applied inside the transaction wrapper used by Explain, ExplainDDL,
// and Execute. A non-positive value falls back to
// DefaultStatementTimeout so callers cannot accidentally disable the
// guardrail by passing a zero duration.
func WithStatementTimeout(d time.Duration) Option {
	return func(m *Manager) {
		if d <= 0 {
			m.stmtTimeout = DefaultStatementTimeout
			return
		}
		m.stmtTimeout = d
	}
}

// NewManager creates a Manager that will connect to the cluster
// identified by dsn on first use. It does not validate or parse the
// DSN — invalid values surface as connection errors on first use.
//
// Options are applied in order; later options override earlier ones.
// Callers that pass no options get DefaultStatementTimeout for the
// txn-wrapper guardrail used by Explain, ExplainDDL, and Execute.
func NewManager(dsn string, opts ...Option) *Manager {
	m := &Manager{
		dsn:         dsn,
		stmtTimeout: DefaultStatementTimeout,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
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
// EXPLAIN (without ANALYZE) does not execute the wrapped statement,
// but as defense-in-depth the call still runs inside a BEGIN READ ONLY
// transaction with SET LOCAL statement_timeout = m.stmtTimeout. The
// txn guarantees that any future shape change (e.g. an EXPLAIN flavor
// that does write) cannot escape the read-only sandbox at this layer.
// The companion AST allowlist in internal/safety is the first line of
// defense and runs before this method is reached. Cluster errors
// (syntax in the wrapped statement, perm denied, etc.) are returned
// wrapped; callers surface them as generic envelope errors today.
// SQLSTATE-aware enrichment for pgwire errors is a future enhancement,
// not a current contract.
//
// On any begin/exec/query/scan/parse failure after a successful
// connect, the underlying connection is closed and the Manager reverts
// to its pre-connect state, mirroring Ping's recovery contract.
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

// runExplain is the inner half of Explain that owns the
// txn/query/scan/parse pipeline. Splitting it out lets Explain
// centralize the connection recovery: any error returned here triggers
// the same close-and-nil sequence in the caller, so the failure modes
// (BeginTx, Exec timeout, Query, Scan, rows.Err, parse, Commit) cannot
// drift apart.
//
// The txn is opened with pgx.ReadOnly so any DML or DDL that somehow
// reaches this method is rejected by the cluster with SQLSTATE 25006
// ("read-only transaction"), mirroring the AST-layer rejection from
// internal/safety.
func (m *Manager) runExplain(ctx context.Context, sql string) (ExplainResult, error) {
	tx, err := m.conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return ExplainResult{}, fmt.Errorf("begin read-only txn: %w", err)
	}
	// Rollback is best-effort and a no-op after a successful Commit;
	// the deferred call exists so any early return below releases the
	// txn rather than leaving it open on the connection (which the
	// caller is about to close on error anyway, but the explicit
	// rollback matches the documented contract for callers reading
	// the code).
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort cleanup

	if _, err := tx.Exec(ctx,
		fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", m.stmtTimeout.Milliseconds())); err != nil {
		return ExplainResult{}, fmt.Errorf("set statement_timeout: %w", err)
	}

	rows, err := tx.Query(ctx, "EXPLAIN "+sql)
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
	// Explicit Close before Commit: pgx requires releasing any open
	// rows handle before reusing the conn for another command. The
	// deferred rows.Close above is harmless when called twice (pgx
	// makes Close idempotent), so the close error here is the same
	// late-driver-error condition the deferred close would surface;
	// either way it would be eclipsed by the Commit attempt below.
	rows.Close() //nolint:errcheck // see comment above

	if err := tx.Commit(ctx); err != nil {
		return ExplainResult{}, fmt.Errorf("commit read-only txn: %w", err)
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
// the declarative schema changer to compile a plan. The call runs
// inside a transaction so SET LOCAL statement_timeout = m.stmtTimeout
// applies, but the txn is NOT opened in pgx.ReadOnly mode: the cluster
// rejects `EXPLAIN (DDL, SHAPE) <ddl>` inside a read-only txn with
// SQLSTATE 25006 ("cannot execute <ddl-tag> in a read-only
// transaction") because the txn-mode check fires on the inner stmt
// type before the SHAPE-only flag is consulted. The AST-layer
// allowlist in internal/safety is the first line of defense for
// rejecting unwanted DDL; statement_timeout is the second.
//
// On any begin/exec/query/scan/parse failure after a successful
// connect, the underlying connection is closed and the Manager reverts
// to its pre-connect state, mirroring Explain's recovery contract.
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
// txn/query/scan/parse pipeline. SHAPE output is contractually a single
// row whose `info` column is the entire multi-line plan; we iterate
// with Query (rather than QueryRow) so we can fail loudly on a future
// CRDB version that splits the output across rows, matching the
// parser's "be strict so format changes surface here" discipline.
// Splitting this out lets ExplainDDL centralize the connection-recovery
// sequence so the failure modes (BeginTx, Exec timeout, Query, Scan,
// rows.Err, multi-row, parse, Commit) cannot drift apart.
//
// Unlike runExplain, the txn is opened in default (read-write) mode:
// the cluster's read-only-txn check fires on the inner DDL stmt before
// the SHAPE-only flag is consulted, so wrapping in pgx.ReadOnly would
// reject every well-formed call with SQLSTATE 25006. The AST-layer
// allowlist in internal/safety is the load-bearing safety check for
// this surface; the txn here exists solely so SET LOCAL
// statement_timeout scopes to this call.
func (m *Manager) runExplainDDL(ctx context.Context, sql string) (DDLExplainResult, error) {
	tx, err := m.conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return DDLExplainResult{}, fmt.Errorf("begin txn: %w", err)
	}
	// Rollback is best-effort and a no-op after Commit. Although this
	// txn is read-write, swallowing the rollback error is safe because
	// the only Exec between BEGIN and Commit is `SET LOCAL` (no data
	// writes), and EXPLAIN (DDL, SHAPE) is documented to compile a
	// plan without executing the wrapped DDL. If a future CRDB version
	// changes either invariant, this nolint deserves revisiting.
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort cleanup

	if _, err := tx.Exec(ctx,
		fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", m.stmtTimeout.Milliseconds())); err != nil {
		return DDLExplainResult{}, fmt.Errorf("set statement_timeout: %w", err)
	}

	rows, err := tx.Query(ctx, "EXPLAIN (DDL, SHAPE) "+sql)
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
	// Explicit Close before Commit: see the matching comment in
	// runExplain. pgx requires releasing the rows handle before
	// reusing the conn, and the deferred Close above is idempotent.
	rows.Close() //nolint:errcheck // see comment above

	if err := tx.Commit(ctx); err != nil {
		return DDLExplainResult{}, fmt.Errorf("commit txn: %w", err)
	}

	text := raw[0]
	statement, operations, err := parseExplainDDLShape(text)
	if err != nil {
		return DDLExplainResult{}, fmt.Errorf("parse EXPLAIN (DDL, SHAPE) output: %w", err)
	}
	return DDLExplainResult{Statement: statement, Operations: operations, RawText: text}, nil
}

// ExplainAnyResult is the discriminated union returned by ExplainAny.
// Strategy names which EXPLAIN flavor the dispatcher chose; on a
// successful call exactly one populated pointer is returned
// (mirroring SimulateStep's per-step result shape so agents that
// already parse simulate output know how to read this).
//
//	strategy = "explain"     → Plan is set    (ExplainResult), DDLPlan is nil.
//	strategy = "explain_ddl" → DDLPlan is set (DDLExplainResult), Plan is nil.
//
// On failure ExplainAny returns the zero value plus a non-nil error,
// so renderers should only read Plan / DDLPlan after checking err.
type ExplainAnyResult struct {
	Strategy Strategy          `json:"strategy"`
	Plan     *ExplainResult    `json:"plan,omitempty"`
	DDLPlan  *DDLExplainResult `json:"ddl_plan,omitempty"`
}

// ExplainAny dispatches a single SQL statement to the right EXPLAIN
// flavor and returns the result keyed by Strategy. SELECT and DML
// route to plain `EXPLAIN <stmt>` (Manager.Explain); DDL routes to
// `EXPLAIN (DDL, SHAPE) <stmt>` (Manager.ExplainDDL). Neither path
// executes the wrapped statement.
//
// Caller contract:
//   - sql must contain exactly one statement. Multi-statement input is
//     rejected with a clear error so the caller migrates to Simulate
//     instead of silently dropping all but the first.
//   - safety.Check(safety.OpExplain, ...) must have run upstream.
//     ExplainAny does not re-validate statement classes; a TCL/DCL
//     input that bypasses the safety gate surfaces here as a "no
//     route" error, which matches simulate's defense-in-depth posture
//     rather than misrouting to one of the two backend methods.
//
// On any error ExplainAny returns the zero result; the underlying
// Manager.Explain / Manager.ExplainDDL handle their own
// connection-recovery sequence so a per-call failure does not leave
// the Manager in a half-open state.
func (m *Manager) ExplainAny(ctx context.Context, sql string) (ExplainAnyResult, error) {
	stmts, err := parser.Parse(sql)
	if err != nil {
		return ExplainAnyResult{}, fmt.Errorf("parse explain input: %w", err)
	}
	if len(stmts) == 0 {
		return ExplainAnyResult{}, errors.New("no statements parsed")
	}
	if len(stmts) > 1 {
		return ExplainAnyResult{}, fmt.Errorf(
			"explain accepts a single statement, got %d (use simulate for multi-statement input)",
			len(stmts))
	}
	ast := stmts[0].AST
	switch ast.StatementType() {
	case tree.TypeDDL:
		ddl, err := m.ExplainDDL(ctx, sql)
		if err != nil {
			return ExplainAnyResult{}, err
		}
		return ExplainAnyResult{Strategy: StrategyExplainDDL, DDLPlan: &ddl}, nil
	case tree.TypeDML:
		plan, err := m.Explain(ctx, sql)
		if err != nil {
			return ExplainAnyResult{}, err
		}
		return ExplainAnyResult{Strategy: StrategyExplain, Plan: &plan}, nil
	default:
		// TCL (BEGIN/COMMIT/ROLLBACK), DCL (GRANT/REVOKE), and other
		// shapes have no EXPLAIN form. The safety gate at OpExplain
		// rejects DCL upstream, but a bypass should fail loudly here
		// rather than misroute to one of the two backend methods.
		return ExplainAnyResult{}, fmt.Errorf(
			"explain has no route for statement type %s", ast.StatementTag())
	}
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
