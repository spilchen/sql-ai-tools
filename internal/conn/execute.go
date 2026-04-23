// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package conn

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/spilchen/sql-ai-tools/internal/safety"
)

// ColumnMeta names a single column in an Execute result. Type is the
// pgtype.Type.Name reported by the driver — best-effort, because the
// pgx type map does not have a registered name for every CRDB-specific
// OID. When the lookup misses, Type is left empty rather than guessed.
type ColumnMeta struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
}

// ExecuteResult is the structured payload from Manager.Execute.
//
// Lifecycle: built once per call by runExecute on the success path;
// consumed once by cmd/exec.go (or the MCP handler) to populate
// Envelope.Data. The fields split along three axes:
//
//   - Result-set shape: Columns + Rows + RowsReturned describe what
//     the cluster handed back. For DML without RETURNING, Columns is
//     empty and Rows is nil — callers should check len(Columns) to
//     decide between "tabular" and "command" rendering.
//
//   - Side-effect summary: RowsAffected and CommandTag mirror the
//     pgwire CommandTag (e.g. "INSERT 0 5"). Always populated, even
//     for SELECTs (RowsAffected matches RowsReturned in that case)
//     and even on the truncation path (runExecute closes the rows
//     handle before reading the tag, so the cluster reports the
//     authoritative count regardless of how many rows we scanned).
//
//   - Guardrail telemetry: LimitInjected is non-nil when the caller
//     ran safety.MaybeInjectLimit and a LIMIT was added; Truncated is
//     true when row scanning hit MaxRows and stopped early. Both let
//     an agent reason about whether the response is complete.
type ExecuteResult struct {
	Columns       []ColumnMeta `json:"columns,omitempty"`
	Rows          [][]any      `json:"rows,omitempty"`
	RowsReturned  int          `json:"rows_returned"`
	RowsAffected  int64        `json:"rows_affected"`
	CommandTag    string       `json:"command_tag,omitempty"`
	LimitInjected *int         `json:"limit_injected,omitempty"`
	Truncated     bool         `json:"truncated,omitempty"`
}

// ExecuteOptions configures a single Execute call. Mode determines the
// transaction shape (read-only wrapper, sql_safe_updates, etc.); the
// AST allowlist gate in internal/safety must already have admitted the
// statement under this mode before Execute is called.
//
// MaxRows caps the number of rows scanned into ExecuteResult.Rows.
// Hitting the cap sets Truncated=true and stops the scan early; the
// statement still runs to completion on the cluster (so any side
// effects of a DML … RETURNING are not undone). A zero or negative
// value disables the cap entirely.
type ExecuteOptions struct {
	Mode    safety.Mode
	MaxRows int
}

// Execute runs sql against the cluster and returns its rows + command
// tag wrapped in an ExecuteResult. The mode in opts selects the txn
// shape; the safety package is the only acceptable source of truth for
// "is this statement permitted under this mode" — Execute does not
// re-classify, it only enforces the cluster-side guardrails (read-only
// txn for read_only, sql_safe_updates for safe_write, statement
// timeout for all modes).
//
// On any begin/exec/query/scan failure after a successful connect, the
// underlying connection is closed and the Manager reverts to its
// pre-connect state, mirroring Explain's recovery contract.
func (m *Manager) Execute(ctx context.Context, sql string, opts ExecuteOptions) (ExecuteResult, error) {
	if err := m.connect(ctx); err != nil {
		return ExecuteResult{}, err
	}

	result, err := m.runExecute(ctx, sql, opts)
	if err != nil {
		m.conn.Close(ctx) //nolint:errcheck // best-effort cleanup
		m.conn = nil
		return ExecuteResult{}, err
	}
	return result, nil
}

// runExecute is the inner half of Execute that owns the
// txn/setup/query/scan pipeline. Splitting it out lets Execute
// centralize the connection-recovery sequence so the failure modes
// (BeginTx, Exec timeout, Exec sql_safe_updates, Query, Scan,
// rows.Err, Commit) cannot drift apart.
//
// Per-mode txn shape:
//
//   - read_only: pgx.ReadOnly access mode. The cluster rejects writes
//     (DML and DDL alike) with SQLSTATE 25006 ("read-only transaction").
//     The AST allowlist already rejects both upstream, so this is
//     defense-in-depth — but it is the same cluster behaviour
//     ExplainDDL has to work around (see manager.go's runExplainDDL),
//     and is why a future caller that bypassed the AST gate still
//     could not write under read_only.
//
//   - safe_write: read-write txn plus SET LOCAL sql_safe_updates = on,
//     so the cluster rejects unqualified UPDATE/DELETE at runtime.
//
//   - full_access: read-write txn with no extra session vars; the
//     statement timeout is the only guardrail. The AST allowlist also
//     does not gate full_access, by design (see classifyFullAccessExecute).
//
// Unknown modes are rejected up front rather than silently falling
// through to a read-write txn — that fall-through would be a privilege
// escalation if a future caller forgot to run safety.ParseMode.
func (m *Manager) runExecute(ctx context.Context, sql string, opts ExecuteOptions) (ExecuteResult, error) {
	txOpts, err := txOptionsForMode(opts.Mode)
	if err != nil {
		return ExecuteResult{}, err
	}

	tx, err := m.conn.BeginTx(ctx, txOpts)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("begin txn: %w", err)
	}
	// Rollback is best-effort and a no-op after a successful Commit.
	// On the error paths it forces a server-side rollback if Commit
	// never ran; on the panic path the deferred rollback releases the
	// txn before the panic unwinds to the CLI/MCP caller's deferred
	// mgr.Close. The errcheck suppression is therefore safe: any
	// rollback failure is either post-Commit (harmless) or about to
	// be eclipsed by the conn teardown that Execute performs on error.
	defer tx.Rollback(ctx) //nolint:errcheck // see comment above

	if _, err := tx.Exec(ctx,
		fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", m.stmtTimeout.Milliseconds())); err != nil {
		return ExecuteResult{}, fmt.Errorf("set statement_timeout: %w", err)
	}

	if opts.Mode == safety.ModeSafeWrite {
		// sql_safe_updates is the cluster-side runtime guard for
		// safe_write: bare UPDATE/DELETE without a WHERE (or LIMIT)
		// fails with a "rejected: UPDATE without WHERE clause"
		// message. Pairs with the AST allowlist's admission of DML to
		// give defense-in-depth.
		if _, err := tx.Exec(ctx, "SET LOCAL sql_safe_updates = on"); err != nil {
			return ExecuteResult{}, fmt.Errorf("set sql_safe_updates: %w", err)
		}
	}

	rows, err := tx.Query(ctx, sql)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("execute statement: %w", err)
	}
	defer rows.Close()

	cols := buildColumnMeta(m.conn, rows.FieldDescriptions())

	out := ExecuteResult{Columns: cols}
	for rows.Next() {
		if opts.MaxRows > 0 && len(out.Rows) >= opts.MaxRows {
			out.Truncated = true
			break
		}
		values, err := rows.Values()
		if err != nil {
			return ExecuteResult{}, fmt.Errorf("scan row: %w", err)
		}
		out.Rows = append(out.Rows, values)
	}

	// Order matters here: pgx populates rows.commandTag inside Close,
	// and a tail wire error (e.g. statement_timeout firing after the
	// last data row) is observable only via rows.Err *after* Close.
	// The natural-end path of rows.Next would call Close internally,
	// but the truncation break above skips that — so we Close
	// unconditionally here, then re-check Err, then read CommandTag.
	// Reading CommandTag before Close on the truncation path would
	// silently produce an empty tag and zero RowsAffected.
	rows.Close() //nolint:errcheck // tail errors surface via rows.Err below
	if err := rows.Err(); err != nil {
		return ExecuteResult{}, fmt.Errorf("read rows: %w", err)
	}

	tag := rows.CommandTag()
	out.RowsReturned = len(out.Rows)
	out.RowsAffected = tag.RowsAffected()
	out.CommandTag = tag.String()

	if err := tx.Commit(ctx); err != nil {
		return ExecuteResult{}, fmt.Errorf("commit txn: %w", err)
	}
	return out, nil
}

// txOptionsForMode picks the pgx transaction options for an Execute
// call. The match is exhaustive on the safety.Mode set so a future
// mode added without updating this switch fails loudly rather than
// defaulting to a read-write (most-permissive) txn — which would be
// silent privilege escalation if a caller bypassed safety.ParseMode.
func txOptionsForMode(mode safety.Mode) (pgx.TxOptions, error) {
	switch mode {
	case safety.ModeReadOnly:
		return pgx.TxOptions{AccessMode: pgx.ReadOnly}, nil
	case safety.ModeSafeWrite, safety.ModeFullAccess:
		return pgx.TxOptions{}, nil
	default:
		return pgx.TxOptions{}, fmt.Errorf("execute: unknown safety mode %q", mode)
	}
}

// buildColumnMeta turns pgx FieldDescriptions into the public
// ColumnMeta slice. Returns nil when fds is empty so the JSON envelope
// omits the columns key for DML without RETURNING. Type names come
// from the connection's pgtype.Map; an unknown OID leaves Type empty
// rather than synthesising a fake name, so consumers can tell the
// difference between "the cluster reported text" and "we don't know".
func buildColumnMeta(conn *pgx.Conn, fds []pgconn.FieldDescription) []ColumnMeta {
	if len(fds) == 0 {
		return nil
	}
	tm := conn.TypeMap()
	cols := make([]ColumnMeta, len(fds))
	for i, fd := range fds {
		cols[i].Name = fd.Name
		if t, ok := tm.TypeForOID(fd.DataTypeOID); ok {
			cols[i].Type = t.Name
		}
	}
	return cols
}
