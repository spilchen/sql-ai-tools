// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/spilchen/sql-ai-tools/internal/conn"
	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/safety"
	"github.com/spilchen/sql-ai-tools/internal/sqlinput"
)

// newSimulateCmd builds the `crdb-sql simulate` subcommand. It
// dispatches each parsed statement to the appropriate non-executing
// EXPLAIN flavor and renders the per-statement outcomes:
//
//   - SELECT (and other read-only DML) → EXPLAIN ANALYZE; runtime
//     stats are real because SELECT has no side effects.
//   - INSERT/UPDATE/DELETE/UPSERT → plain EXPLAIN; planner estimates
//     only, no execution, no leaked writes.
//   - DDL → EXPLAIN (DDL, SHAPE) plus a SHOW STATISTICS row-count
//     annotation per affected table; the schema changer compiles a
//     plan but does not apply it.
//
// Tier 3 (connected). The safety allowlist (internal/safety) admits
// every dispatchable statement under the default read_only mode —
// the dispatcher itself is the safety boundary, since none of the
// chosen EXPLAIN flavors execute the inner write or DDL at the
// cluster level. Statement classes the dispatcher has no route for
// (TCL, DCL, nested EXPLAIN) are rejected by the allowlist before
// any cluster contact.
func newSimulateCmd(state *rootState) *cobra.Command {
	var (
		expr    string
		mode    string
		timeout time.Duration
	)

	cmd := &cobra.Command{
		Use:   "simulate [file]",
		Short: "Simulate one or more SQL statements via EXPLAIN flavors that do not execute",
		Long: `Connect to a CockroachDB cluster and simulate the supplied SQL by
dispatching each statement to a non-executing EXPLAIN flavor:

  - SELECT runs through EXPLAIN ANALYZE (real runtime stats; reads
    have no side effects).
  - INSERT/UPDATE/DELETE/UPSERT runs through plain EXPLAIN (planner
    estimates only — the write is never applied).
  - DDL runs through EXPLAIN (DDL, SHAPE) plus SHOW STATISTICS for
    each affected table (the schema change is planned but not
    applied).

Input is read from the -e flag (inline SQL), a positional file
argument, or stdin. Multi-statement input returns one entry per
statement in the order parsed. The connection string is read from
--dsn or CRDB_DSN (flag wins).

Because no flavor executes the inner write or DDL at the cluster
level, the default --mode=read_only admits every dispatched class.
Statement types with no EXPLAIN form (BEGIN/COMMIT, GRANT/REVOKE)
are rejected before any cluster contact.

Pasted output from a cockroach sql REPL session is auto-cleaned: primary
prompts (user@host:port/db>) and continuation prompts (->) are stripped
before parsing, and the JSON envelope carries an input_preprocessed
warning so the modification is visible.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, baseEnv, err := newEnvelope(state, output.TierConnected, cmd)
			if err != nil {
				return r.RenderError(baseEnv, err)
			}

			sql, err := sqlinput.ReadSQL(expr, args, cmd.InOrStdin())
			if err != nil {
				return r.RenderError(baseEnv, err)
			}
			sql = strings.TrimSpace(sql)
			originalSQL := sql
			strip := preprocessSQL(&baseEnv, sql)
			sql = strip.Stripped

			if state.dsn == "" {
				return r.RenderError(baseEnv,
					fmt.Errorf("no connection string: pass --dsn or set CRDB_DSN"))
			}

			parsedMode, err := safety.ParseMode(mode)
			if err != nil {
				return r.RenderError(baseEnv, err)
			}

			// Safety check runs before any cluster contact: a
			// rejection produces a structured envelope with
			// connection_status=disconnected, exactly as if --dsn
			// were missing. Parse errors propagate as parse
			// diagnostics so the agent sees the SQLSTATE-tagged
			// syntax message rather than a misleading safety
			// violation.
			violation, err := safety.Check(parsedMode, safety.OpSimulate, sql)
			if err != nil {
				parseErr := diag.FromParseError(err, sql)
				if strip.Removed {
					parseErr.Position = diag.AdjustPosition(parseErr.Position, originalSQL, strip.Translate)
				}
				return r.RenderErrorEntry(baseEnv, err, parseErr)
			}
			if violation != nil {
				safetyErr := safety.Envelope(violation)
				// safety.Envelope carries no Position today, so the
				// branch below is a no-op. Run it to stay future-proof
				// if safety later attaches positions, matching the
				// pattern in the MCP-side handler and cmd/exec.go.
				if strip.Removed {
					safetyErr.Position = diag.AdjustPosition(safetyErr.Position, originalSQL, strip.Translate)
				}
				return r.RenderErrorEntry(baseEnv,
					fmt.Errorf("safety violation: %s", violation.Reason),
					safetyErr)
			}

			mgr := conn.NewManager(state.dsn, conn.WithStatementTimeout(timeout))
			defer mgr.Close(cmd.Context()) //nolint:errcheck // best-effort cleanup

			result, err := mgr.Simulate(cmd.Context(), sql)
			if err != nil {
				clusterErr := diag.FromClusterError(err, sql)
				if strip.Removed {
					clusterErr.Position = diag.AdjustPosition(clusterErr.Position, originalSQL, strip.Translate)
				}
				return r.RenderErrorEntry(baseEnv, err, clusterErr)
			}

			baseEnv.ConnectionStatus = output.ConnectionConnected

			data, err := json.Marshal(result)
			if err != nil {
				return r.RenderError(baseEnv, err)
			}
			baseEnv.Data = data

			// Surface step-level failures (plan errors and stats
			// errors alike) as a single envelope warning so JSON
			// consumers that read only the top-level errors[] see
			// the partial failure, and so the CLI returns a non-zero
			// exit. The text/JSON body must still render the per-step
			// data — we want the agent to see WHICH step failed and
			// which steps succeeded, not just a summary. Following
			// the renderValidateFailure pattern in cmd/validate.go:
			// append to env.Errors, render the body normally, then
			// return ErrRendered so cmd/crdb-sql/main.go signals failure
			// without reprinting the error.
			if entry, ok := simulateStepWarning(result); ok {
				baseEnv.Errors = append(baseEnv.Errors, entry)
				if err := r.Render(baseEnv, func(w io.Writer) error {
					return renderSimulateText(w, result)
				}); err != nil {
					return err
				}
				return output.ErrRendered
			}

			return r.Render(baseEnv, func(w io.Writer) error {
				return renderSimulateText(w, result)
			})
		},
	}

	cmd.Flags().StringVarP(&expr, "expression", "e", "", "inline SQL to simulate")
	cmd.Flags().StringVar(&mode, "mode", string(safety.DefaultMode),
		"safety mode: read_only (default), safe_write, full_access")
	cmd.Flags().DurationVar(&timeout, "timeout", conn.DefaultStatementTimeout,
		"statement timeout for each wrapped EXPLAIN call")

	return cmd
}

// renderSimulateText renders a SimulateResult for --output=text.
// Each step is preceded by a one-line header naming the strategy
// and statement tag so a multi-statement output stays
// human-readable; the body reuses the EXPLAIN/EXPLAIN (DDL, SHAPE)
// raw text the cluster returned, matching what `cockroach sql`
// would print. Plan errors short-circuit the body. Stats errors
// (DDL plan succeeded but SHOW STATISTICS failed for one or more
// targets) are surfaced after the plan so the operator still sees
// the schema-change steps the simulation produced.
func renderSimulateText(w io.Writer, result conn.SimulateResult) error {
	for i, step := range result.Steps {
		if i > 0 {
			if _, err := io.WriteString(w, "\n"); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "-- step %d: %s (%s)\n",
			step.StatementIndex, step.Tag, step.Strategy); err != nil {
			return err
		}
		if step.Error != "" {
			if _, err := fmt.Fprintf(w, "error: %s\n", step.Error); err != nil {
				return err
			}
			continue
		}
		switch step.Strategy {
		case conn.StrategyExplainDDL:
			if step.DDLPlan != nil {
				if _, err := io.WriteString(w, step.DDLPlan.RawText); err != nil {
					return err
				}
				if !strings.HasSuffix(step.DDLPlan.RawText, "\n") {
					if _, err := io.WriteString(w, "\n"); err != nil {
						return err
					}
				}
			}
			for _, ts := range step.TableStats {
				if ts.CollectedAt == "" {
					// No stats have been collected for this target
					// yet (typically a freshly created table). The
					// row_count field is the zero value; render it
					// as "unavailable" rather than "0" so a fresh
					// table is not mistaken for an empty one.
					if _, err := fmt.Fprintf(w,
						"-- table stats: %s.%s row_count=unavailable\n",
						ts.Schema, ts.Table); err != nil {
						return err
					}
					continue
				}
				if _, err := fmt.Fprintf(w,
					"-- table stats: %s.%s row_count=%d (collected %s)\n",
					ts.Schema, ts.Table, ts.RowCount, ts.CollectedAt); err != nil {
					return err
				}
			}
			if step.StatsError != "" {
				if _, err := fmt.Fprintf(w,
					"-- table stats error: %s\n", step.StatsError); err != nil {
					return err
				}
			}
		default:
			if step.Plan != nil {
				for _, row := range step.Plan.RawRows {
					if _, err := fmt.Fprintln(w, row); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// simulateStepWarning summarises per-step failures into a single
// envelope error entry. Returns ok=false when every step succeeded;
// otherwise builds a `simulate_step_failure` entry whose message
// names which step indices failed and how, and whose Context carries
// the index slices as machine-readable lists. Used by both the CLI
// (to drive a non-zero exit) and the MCP handler (to populate
// env.Errors so agents reading the standard envelope contract see
// the failure).
//
// The Context fields let an agent retry only the failed steps
// without parsing the human-readable message — important because
// the message format can shift across versions, but the field
// names ("plan_failed_steps", "stats_failed_steps") are part of
// the wire contract.
func simulateStepWarning(result conn.SimulateResult) (output.Error, bool) {
	msg, planFails, statsFails, ok := result.StepFailureSummary()
	if !ok {
		return output.Error{}, false
	}
	ctx := make(map[string]any, 2)
	if len(planFails) > 0 {
		ctx["plan_failed_steps"] = planFails
	}
	if len(statsFails) > 0 {
		ctx["stats_failed_steps"] = statsFails
	}
	return output.Error{
		Code:     "simulate_step_failure",
		Severity: output.SeverityError,
		Message:  msg,
		Category: "simulate",
		Context:  ctx,
	}, true
}
