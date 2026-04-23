// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package conn

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/tree"
	"github.com/jackc/pgx/v5"
)

// Strategy names the EXPLAIN flavor a SimulateStep used to render its
// no-execute view of a single statement. The values are wire-stable
// tokens — agents branch on them to decide which payload field to
// read (Plan vs DDLPlan) and how to interpret the numbers (estimates
// vs measured stats).
type Strategy string

// Strategy values.
const (
	// StrategyExplainAnalyze runs `EXPLAIN ANALYZE <stmt>`. Used for
	// SELECT, where actual execution is harmless and the runtime
	// stats (rows read, network bytes, time) are far more useful
	// than the optimizer's estimates.
	StrategyExplainAnalyze Strategy = "explain_analyze"

	// StrategyExplain runs plain `EXPLAIN <stmt>`. Used for DML
	// writes (INSERT/UPDATE/DELETE/UPSERT) where ANALYZE would
	// persist data. Returns the optimizer's estimated plan only —
	// no execution, no side effects.
	StrategyExplain Strategy = "explain"

	// StrategyExplainDDL runs `EXPLAIN (DDL, SHAPE) <stmt>`. Used
	// for DDL. Returns the declarative schema changer's compiled
	// plan; the cluster does not execute the schema change.
	StrategyExplainDDL Strategy = "explain_ddl"
)

// SimulateResult is the JSON-serialisable payload returned by
// Manager.Simulate. One Steps entry per parsed statement, in
// statement order. Errors that scope to a single statement live on
// the step (Step.Error / Step.StatsError); any failure that aborts
// the whole simulation (parse error, connect error) is returned as
// the method-level error instead.
type SimulateResult struct {
	Steps []SimulateStep `json:"steps"`
}

// StepFailureSummary scans every Step and returns a one-line
// summary of any per-step failures plus the indices that carried
// each failure class. Returns ok=false when every step succeeded
// — both surfaces (CLI and MCP) use that to decide whether to
// promote the failure into an envelope-level entry. Keeping the
// summary close to the data types means new step-level error
// classes can be added in one place rather than duplicated across
// surfaces.
func (r SimulateResult) StepFailureSummary() (msg string, planFails, statsFails []int, ok bool) {
	for _, step := range r.Steps {
		if step.Error != "" {
			planFails = append(planFails, step.StatementIndex)
		}
		if step.StatsError != "" {
			statsFails = append(statsFails, step.StatementIndex)
		}
	}
	if len(planFails) == 0 && len(statsFails) == 0 {
		return "", nil, nil, false
	}
	parts := make([]string, 0, 2)
	if len(planFails) > 0 {
		parts = append(parts, fmt.Sprintf("%d plan error(s) at step(s) %v", len(planFails), planFails))
	}
	if len(statsFails) > 0 {
		parts = append(parts, fmt.Sprintf("%d stats error(s) at step(s) %v", len(statsFails), statsFails))
	}
	return strings.Join(parts, "; ") + " (see data.steps for per-step detail)",
		planFails, statsFails, true
}

// SimulateStep records the simulated outcome for a single statement.
// Exactly one of Plan / DDLPlan is non-nil on success, selected by
// Strategy:
//
//	explain_analyze | explain → Plan is set, DDLPlan is nil.
//	explain_ddl              → DDLPlan is set, Plan is nil.
//
// On a plan failure (cluster rejected EXPLAIN, statement timeout,
// connection drop after the dispatch began), Plan and DDLPlan are
// both nil and Error carries the message. On a stats-only failure
// (DDL plan succeeded but SHOW STATISTICS errored for one of the
// affected tables), DDLPlan stays populated and StatsError carries
// the lookup failure — keeping Error reserved for plan-blocking
// problems lets renderers surface the plan and the stats failure
// independently. Subsequent steps still run regardless of which
// field is set; failures do not abort the simulation.
type SimulateStep struct {
	StatementIndex int      `json:"statement_index"`
	Tag            string   `json:"tag"`
	Strategy       Strategy `json:"strategy"`
	SQL            string   `json:"sql"`

	Plan       *ExplainResult    `json:"plan,omitempty"`
	DDLPlan    *DDLExplainResult `json:"ddl_plan,omitempty"`
	TableStats []TableStat       `json:"table_stats,omitempty"`

	Error      string `json:"error,omitempty"`
	StatsError string `json:"stats_error,omitempty"`
}

// TableStat is a row-count estimate for a table touched by a
// simulated DDL. Sourced from `SHOW STATISTICS` (auto-collected by
// CRDB), so the numbers reflect the most recent stats refresh rather
// than a live count. CollectedAt lets callers reason about
// staleness; an empty CollectedAt means stats have never been
// collected for this table — RowCount in that case is the zero
// value and is not meaningful.
//
// The slice on SimulateStep is omitted from the JSON payload (via
// `omitempty`) when the DDL has no extractable target table or when
// stats lookup failed for every target. Callers should treat both
// "absent slice" and "non-empty slice with empty CollectedAt" as
// "no information available," not as "row count = 0".
type TableStat struct {
	Schema      string `json:"schema"`
	Table       string `json:"table"`
	RowCount    int64  `json:"row_count"`
	Source      string `json:"source"`
	CollectedAt string `json:"collected_at,omitempty"`
}

// Simulate parses sql, dispatches each statement to the appropriate
// non-executing EXPLAIN flavor, and returns the per-statement
// outcomes. The dispatcher is what makes simulate side-effect free
// at the cluster level: SELECT runs through EXPLAIN ANALYZE (read
// only by construction), DML writes through plain EXPLAIN (no
// execution), and DDL through EXPLAIN (DDL, SHAPE) (no execution
// plus an optional row-count annotation from SHOW STATISTICS).
//
// Per-statement errors land on the step rather than the method:
// plan failures populate step.Error, stats-only failures populate
// step.StatsError. Subsequent steps still run regardless. Errors
// that abort the whole call (parse failure, initial connect) are
// returned as the method-level error.
//
// Caller contract: safety.Check(safety.OpSimulate, ...) must have
// been called before Simulate. Simulate does not re-validate
// statement classes. nested EXPLAIN must be rejected upstream
// because tree.CanWriteData does not descend into Explain wrappers
// — the dispatcher would misclassify a wrapped write as a SELECT
// and route it to EXPLAIN ANALYZE. TCL and DCL would surface as a
// per-step "no route" error in the default branch, which is
// actionable but not the intended path. Callers must walk Steps for
// per-step Error/StatsError values; a nil method-level error does
// not mean every statement succeeded.
func (m *Manager) Simulate(ctx context.Context, sql string) (SimulateResult, error) {
	stmts, err := parser.Parse(sql)
	if err != nil {
		return SimulateResult{}, fmt.Errorf("parse simulate input: %w", err)
	}
	if len(stmts) == 0 {
		return SimulateResult{}, errors.New("no statements parsed")
	}
	// Fail fast on connect: a per-statement loop that re-dials on
	// every step would record the same connect error N times. The
	// caller wants one method-level error, rendered the same way as
	// any other Tier 3 connect failure.
	if err := m.connect(ctx); err != nil {
		return SimulateResult{}, err
	}

	steps := make([]SimulateStep, 0, len(stmts))
	for i, s := range stmts {
		step := SimulateStep{
			StatementIndex: i,
			Tag:            s.AST.StatementTag(),
			SQL:            s.SQL,
		}
		m.dispatchSimulateStep(ctx, s.AST, s.SQL, &step)
		steps = append(steps, step)
	}
	return SimulateResult{Steps: steps}, nil
}

// dispatchSimulateStep picks the EXPLAIN flavor for ast, runs it,
// and populates step in place. The dispatcher branches on the AST,
// not on the SQL text, so wrapper noise (comments, whitespace) does
// not affect routing. Any error is recorded on step.Error rather
// than returned, so the caller can keep iterating over remaining
// statements.
func (m *Manager) dispatchSimulateStep(
	ctx context.Context, ast tree.Statement, sql string, step *SimulateStep,
) {
	switch ast.StatementType() {
	case tree.TypeDDL:
		step.Strategy = StrategyExplainDDL
		ddl, err := m.ExplainDDL(ctx, sql)
		if err != nil {
			step.Error = err.Error()
			return
		}
		step.DDLPlan = &ddl
		// Stats lookup is best-effort: a missing table or absent
		// stats should not blank the plan we already have, nor
		// should a single bad target wipe the partial successes.
		// collectDDLTableStats returns whatever it managed to
		// collect plus any failure message; both fields land on
		// the step independently, so renderers can surface the
		// plan, the partial stats, and the lookup error all at
		// once.
		step.TableStats, step.StatsError = m.collectDDLTableStats(ctx, ast)
	case tree.TypeDML:
		if tree.CanWriteData(ast) {
			step.Strategy = StrategyExplain
			plan, err := m.Explain(ctx, sql)
			if err != nil {
				step.Error = err.Error()
				return
			}
			step.Plan = &plan
			return
		}
		step.Strategy = StrategyExplainAnalyze
		plan, err := m.ExplainAnalyze(ctx, sql)
		if err != nil {
			step.Error = err.Error()
			return
		}
		step.Plan = &plan
	default:
		// safety.Check(OpSimulate) is supposed to reject TCL and
		// DCL before we get here (nested EXPLAIN reports as
		// TypeDML and so would not land in this branch — that's
		// the case the safety gate must catch upstream because the
		// dispatcher cannot detect a wrapped write). This branch
		// exists so a bypass of the TCL/DCL rule surfaces as an
		// actionable per-step error instead of a panic or a
		// misleading cluster reject.
		step.Error = fmt.Sprintf("simulate has no route for statement type %s", ast.StatementTag())
	}
}

// ExplainAnalyze runs `EXPLAIN ANALYZE <sql>` against the cluster
// and returns the parsed plan tree alongside the raw tabular output.
// EXPLAIN ANALYZE physically executes the wrapped statement, so the
// returned Plan carries measured runtime stats (rows read, network
// bytes, execution time) rather than the optimizer's estimates.
//
// Caller contract: sql must be a SELECT (or other read-only DML
// shape — VALUES, WITH, SHOW). The dispatcher in Simulate enforces
// this by routing CanWriteData statements to plain Explain instead.
// As defense in depth, the call still runs inside BEGIN READ ONLY
// with a SET LOCAL statement_timeout, so any write that reaches
// this method is rejected by the cluster with SQLSTATE 25006 and
// the timeout caps slow plans.
//
// On any begin/exec/query/scan/parse failure after a successful
// connect, the underlying connection is closed and the Manager
// reverts to its pre-connect state, mirroring Explain's recovery
// contract.
func (m *Manager) ExplainAnalyze(ctx context.Context, sql string) (ExplainResult, error) {
	if err := m.connect(ctx); err != nil {
		return ExplainResult{}, err
	}

	result, err := m.runExplainAnalyze(ctx, sql)
	if err != nil {
		m.conn.Close(ctx) //nolint:errcheck // best-effort cleanup
		m.conn = nil
		return ExplainResult{}, err
	}
	return result, nil
}

// runExplainAnalyze owns the txn/query/scan/parse pipeline for
// ExplainAnalyze. Splitting it out lets ExplainAnalyze centralize
// the connection-recovery sequence so the failure modes (BeginTx,
// Exec timeout, Query, Scan, rows.Err, parse, Commit) cannot drift
// apart from the runExplain twin.
func (m *Manager) runExplainAnalyze(ctx context.Context, sql string) (ExplainResult, error) {
	tx, err := m.conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return ExplainResult{}, fmt.Errorf("begin read-only txn: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort cleanup

	if _, err := tx.Exec(ctx,
		fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", m.stmtTimeout.Milliseconds())); err != nil {
		return ExplainResult{}, fmt.Errorf("set statement_timeout: %w", err)
	}

	rows, err := tx.Query(ctx, "EXPLAIN ANALYZE "+sql)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("run EXPLAIN ANALYZE: %w", err)
	}
	defer rows.Close()

	var raw []string
	for rows.Next() {
		var info string
		if err := rows.Scan(&info); err != nil {
			return ExplainResult{}, fmt.Errorf("scan EXPLAIN ANALYZE row: %w", err)
		}
		raw = append(raw, info)
	}
	if err := rows.Err(); err != nil {
		return ExplainResult{}, fmt.Errorf("read EXPLAIN ANALYZE rows: %w", err)
	}
	rows.Close() //nolint:errcheck // pgx requires release before commit; idempotent

	if err := tx.Commit(ctx); err != nil {
		return ExplainResult{}, fmt.Errorf("commit read-only txn: %w", err)
	}

	header, plan, err := parseExplainTree(raw)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("parse EXPLAIN ANALYZE output: %w", err)
	}
	return ExplainResult{Header: header, Plan: plan, RawRows: raw}, nil
}

// GetTableStats returns the most recent row-count statistic for the
// table at (schema, table). The source is SHOW STATISTICS, which
// CRDB auto-collects: typically there is one row per (schema,
// table, column-set) and the row with the latest `created`
// timestamp wins. We pick the highest row_count across all column
// sets — every column set's stats sample the same physical table,
// so they should agree, and picking the max is a forgiving choice
// when a single set is briefly stale.
//
// An empty schema is allowed and means "resolve against the
// connection's search_path." That matches how the same SQL would
// behave if the user ran the DDL in this connection, which is the
// resolution the simulation should report against. Callers that
// need a deterministic schema should pass it explicitly.
//
// Returns a zero TableStat (no error) when stats have not been
// collected yet — that is the common case for freshly created
// tables and is not an error condition the caller needs to
// distinguish from a missing table. A non-nil error is reserved
// for actual cluster failures.
func (m *Manager) GetTableStats(ctx context.Context, schema, table string) (TableStat, error) {
	if err := m.connect(ctx); err != nil {
		return TableStat{}, err
	}

	stat, err := m.runGetTableStats(ctx, schema, table)
	if err != nil {
		m.conn.Close(ctx) //nolint:errcheck // best-effort cleanup
		m.conn = nil
		return TableStat{}, err
	}
	return stat, nil
}

// runGetTableStats is the inner half of GetTableStats. SHOW
// STATISTICS does not accept placeholders for the table identifier,
// so the table (and optional schema) are escaped via
// pgx.Identifier.Sanitize to prevent injection.
func (m *Manager) runGetTableStats(ctx context.Context, schema, table string) (TableStat, error) {
	if table == "" {
		return TableStat{}, fmt.Errorf("table must not be empty")
	}
	var qualified string
	if schema == "" {
		qualified = pgx.Identifier{table}.Sanitize()
	} else {
		qualified = pgx.Identifier{schema, table}.Sanitize()
	}
	// `created` ordering picks the most recent stat collection;
	// `row_count DESC` breaks ties by preferring the higher number
	// (every column set samples the same table, so equality is the
	// expected case but not guaranteed across collection windows).
	query := "SELECT row_count, created::STRING FROM [SHOW STATISTICS FOR TABLE " + qualified +
		"] ORDER BY created DESC, row_count DESC LIMIT 1"

	var (
		rowCount    int64
		collectedAt string
	)
	err := m.conn.QueryRow(ctx, query).Scan(&rowCount, &collectedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No stats have been collected yet — return zero.
			return TableStat{Schema: schema, Table: table, Source: "show_statistics"}, nil
		}
		return TableStat{}, fmt.Errorf("SHOW STATISTICS FOR TABLE %s: %w", qualified, err)
	}
	return TableStat{
		Schema:      schema,
		Table:       table,
		RowCount:    rowCount,
		Source:      "show_statistics",
		CollectedAt: collectedAt,
	}, nil
}

// collectDDLTableStats walks ast for table targets and returns one
// TableStat per successfully looked-up target plus a non-empty
// error string when at least one target failed. Statements with no
// extractable target (e.g. CREATE SCHEMA, CREATE DATABASE) yield a
// nil slice and an empty error — "nothing to annotate" is the
// correct answer.
//
// Partial successes are preserved: a DROP TABLE a, b, c where the
// stats lookup for `b` errors still returns the stats for `a` (and
// `c`), with the StatsError message listing every failed target.
// We deliberately accumulate ALL per-target errors rather than
// keeping only the first — different targets can hit different
// failure classes (permission denied on one, undefined relation on
// another) and an operator debugging the simulation needs to see
// each one to act on it.
func (m *Manager) collectDDLTableStats(ctx context.Context, ast tree.Statement) ([]TableStat, string) {
	targets := ddlTargets(ast)
	if len(targets) == 0 {
		return nil, ""
	}
	stats := make([]TableStat, 0, len(targets))
	var failures []string
	for _, t := range targets {
		stat, err := m.GetTableStats(ctx, t.Schema, t.Table)
		if err != nil {
			label := t.Table
			if t.Schema != "" {
				label = t.Schema + "." + t.Table
			}
			failures = append(failures, fmt.Sprintf("%s: %v", label, err))
			continue
		}
		stats = append(stats, stat)
	}
	if len(stats) == 0 {
		stats = nil
	}
	return stats, strings.Join(failures, "; ")
}

// ddlTarget identifies a (schema, table) pair touched by a DDL
// statement. Schema is the parser-supplied qualifier and may be
// empty when the user wrote an unqualified name (e.g.
// `ALTER TABLE users ...`); GetTableStats handles the empty case
// by letting SHOW STATISTICS resolve against the connection's
// search_path, which is the same resolution the eventual DDL would
// see on this connection.
type ddlTarget struct {
	Schema string
	Table  string
}

// ddlTargets extracts table targets from the DDL forms simulate
// supports today: ALTER TABLE, CREATE INDEX, DROP INDEX, DROP
// TABLE. Other forms (CREATE TABLE on a brand-new name, CREATE
// SCHEMA, CREATE DATABASE, etc.) yield no targets — there is no
// pre-existing table whose row count would inform the simulation.
//
// Adding a new DDL form is a one-case extension here. We chose
// case-by-case extraction over a generic AST walker so the cost of
// each new form is explicit and the helper stays auditable.
func ddlTargets(stmt tree.Statement) []ddlTarget {
	switch s := stmt.(type) {
	case *tree.AlterTable:
		tn := s.Table.ToTableName()
		return []ddlTarget{{Schema: tn.Schema(), Table: tn.Table()}}
	case *tree.CreateIndex:
		return []ddlTarget{{Schema: s.Table.Schema(), Table: s.Table.Table()}}
	case *tree.DropTable:
		out := make([]ddlTarget, 0, len(s.Names))
		for i := range s.Names {
			out = append(out, ddlTarget{Schema: s.Names[i].Schema(), Table: s.Names[i].Table()})
		}
		return out
	case *tree.DropIndex:
		out := make([]ddlTarget, 0, len(s.IndexList))
		for _, idx := range s.IndexList {
			out = append(out, ddlTarget{Schema: idx.Table.Schema(), Table: idx.Table.Table()})
		}
		return out
	default:
		return nil
	}
}

// simulateStrategyForAST is a package-private helper so tests can
// verify dispatch decisions without round-tripping through Simulate
// (which would require a live cluster). Returns the strategy a real
// Simulate call would pick for ast, plus a boolean indicating
// whether the dispatcher has any route at all.
func simulateStrategyForAST(ast tree.Statement) (Strategy, bool) {
	switch ast.StatementType() {
	case tree.TypeDDL:
		return StrategyExplainDDL, true
	case tree.TypeDML:
		if tree.CanWriteData(ast) {
			return StrategyExplain, true
		}
		return StrategyExplainAnalyze, true
	default:
		return "", false
	}
}
