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
	"text/tabwriter"
	"time"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/spf13/cobra"

	"github.com/spilchen/sql-ai-tools/internal/conn"
	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/safety"
	"github.com/spilchen/sql-ai-tools/internal/sqlinput"
	"github.com/spilchen/sql-ai-tools/internal/version"
)

// defaultExecMaxRows is the row cap applied to execute results when
// the user does not override --max-rows. Picked to be large enough
// for typical agent-driven analysis while still bounding a runaway
// SELECT * or a wide RETURNING set. The cap drives runtime row-scan
// truncation in conn.Execute on every mode, and additionally drives
// AST-level LIMIT injection on read_only SELECTs (only that mode
// gets the rewrite, because non-read_only callers have explicitly
// opted into writes / unbounded scans).
//
// The same default lives in internal/mcp/tools/execute.go
// (defaultExecuteMaxRows); the two are intentionally not shared via
// a common package so the cmd layer does not pull in
// internal/mcp/tools (the MCP tool implementations) just to read a
// constant. They must be kept in sync by hand.
const defaultExecMaxRows = 1000

// newExecCmd builds the `crdb-sql exec` subcommand. It runs the
// supplied SQL against the cluster identified by --dsn (or the
// CRDB_DSN env var) and renders the rows + command tag in the same
// envelope shape used by the other Tier 3 commands.
//
// Safety policy is selected by --mode (default read_only). The flow:
//
//  1. Parse the mode and the input SQL (sqlinput allows -e, file arg,
//     or stdin).
//  2. Run safety.Check before any cluster contact. A violation produces
//     a structured envelope with connection_status=disconnected.
//  3. For read_only SELECTs without a LIMIT, inject one bounded by
//     --max-rows so an unbounded SELECT * cannot blow up the response.
//  4. Hand off to conn.Manager.Execute, which applies the per-mode
//     transaction wrapper (read-only txn, sql_safe_updates, statement
//     timeout) for cluster-side defense-in-depth.
//
// Demo (matches issue #29):
//
//	crdb-sql exec -e "SELECT * FROM users"             — succeeds (read_only)
//	crdb-sql exec -e "DELETE FROM users"               — safety_violation
//	crdb-sql exec --mode safe_write -e "DELETE FROM users WHERE id=1"
//	                                                     — succeeds, prints DELETE 1
func newExecCmd(state *rootState) *cobra.Command {
	var (
		expr    string
		mode    string
		timeout time.Duration
		maxRows int
	)

	cmd := &cobra.Command{
		Use:   "exec [file]",
		Short: "Execute SQL against the cluster with safety guardrails",
		Long: `Connect to a CockroachDB cluster and execute the supplied SQL. Input
is read from the -e flag (inline SQL), a positional file argument, or
stdin. The connection string is read from --dsn or CRDB_DSN (flag wins).

The --mode flag selects the safety policy applied before the cluster
call:

  read_only   (default) admits SELECT/SHOW/EXPLAIN and other
              non-mutating statements; rejects writes and DDL.
  safe_write  also admits INSERT/UPDATE/UPSERT/DELETE; rejects DDL
              and privilege changes. The cluster also enforces
              sql_safe_updates so unqualified UPDATE/DELETE fail at
              runtime.
  full_access admits any statement that parses; the only guardrail
              is the statement timeout.

For read_only SELECTs that lack a LIMIT clause, --max-rows is injected
into the rewritten SQL so the cluster does not stream an unbounded
result. Set --max-rows=0 to disable both injection and runtime
truncation.

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

			// Parse once up front so version.Inspect, safety.CheckParsed,
			// and safety.MaybeInjectLimitParsed share a single AST. The
			// cluster will reparse on its own — that's the cluster's
			// concern — but the client-side parses collapse to one.
			//
			// Order matters: version.Inspect and safety.CheckParsed are
			// read-only walks; safety.MaybeInjectLimitParsed mutates
			// stmts[0].AST.Limit in place when injection fires, so the
			// inspectors must run first.
			parsed, parseErr := parser.Parse(sql)
			if parseErr != nil {
				return renderParseErrorTranslated(r, baseEnv, parseErr, sql, originalSQL, strip)
			}
			baseEnv.Errors = append(baseEnv.Errors,
				version.Inspect(parsed, state.targetVersion, nil)...)

			if violation := safety.CheckParsed(parsedMode, safety.OpExecute, parsed); violation != nil {
				safetyErr := safety.Envelope(violation)
				// safety.Envelope carries no Position today, so the
				// branch below is a no-op. Run it to stay future-proof
				// if safety later attaches positions, matching the
				// pattern used by cmd/explain.go, cmd/explain_ddl.go,
				// and cmd/simulate.go.
				if strip.Removed {
					safetyErr.Position = diag.AdjustPosition(safetyErr.Position, originalSQL, strip.Translate)
				}
				return r.RenderErrorEntry(baseEnv,
					fmt.Errorf("safety violation: %s", violation.Reason),
					safetyErr)
			}

			// LIMIT injection is scoped to read_only because the other
			// modes are explicit opt-ins where the user has already
			// accepted writes / unbounded scans.
			rewritten := sql
			var injected bool
			if parsedMode == safety.ModeReadOnly && maxRows > 0 {
				if rw, did := safety.MaybeInjectLimitParsed(parsed, maxRows); did {
					rewritten = rw
					injected = true
				}
			}

			mgr := conn.NewManager(state.dsn, conn.WithStatementTimeout(timeout))
			defer mgr.Close(cmd.Context()) //nolint:errcheck // best-effort cleanup

			result, err := mgr.Execute(cmd.Context(), rewritten, conn.ExecuteOptions{
				Mode:    parsedMode,
				MaxRows: maxRows,
			})
			if err != nil {
				clusterErr := diag.FromClusterError(err, rewritten)
				// Order matters: when both `injected` and
				// `strip.Removed` are true, `injected` must win because
				// the canonicalized rewrite invalidates the strip map.
				switch {
				case injected:
					// rewritten is the canonicalized AST re-serialized by
					// tree.AsStringWithFlags, not stripped SQL with an
					// appended LIMIT. Pgwire positions index into rewritten,
					// so strip.Translate (stripped → original) cannot
					// honestly translate them. Drop Position rather than
					// report wrong line/column.
					clusterErr.Position = nil
					if clusterErr.Context == nil {
						clusterErr.Context = make(map[string]any, 1)
					}
					clusterErr.Context[output.ContextPositionOmittedReason] = output.ReasonLimitInjectionRewroteSQL
				case strip.Removed:
					clusterErr.Position = diag.AdjustPosition(clusterErr.Position, originalSQL, strip.Translate)
				}
				return r.RenderErrorEntry(baseEnv, err, clusterErr)
			}

			if injected {
				limit := maxRows
				result.LimitInjected = &limit
			}

			baseEnv.ConnectionStatus = output.ConnectionConnected

			data, err := json.Marshal(result)
			if err != nil {
				return r.RenderError(baseEnv, err)
			}
			baseEnv.Data = data

			return r.Render(baseEnv, func(w io.Writer) error {
				return renderExecText(w, result)
			})
		},
	}

	cmd.Flags().StringVarP(&expr, "expression", "e", "", "inline SQL to execute")
	cmd.Flags().StringVar(&mode, "mode", string(safety.DefaultMode),
		"safety mode: read_only (default), safe_write, full_access")
	cmd.Flags().DurationVar(&timeout, "timeout", conn.DefaultStatementTimeout,
		"statement timeout for the executed call")
	cmd.Flags().IntVar(&maxRows, "max-rows", defaultExecMaxRows,
		"maximum rows returned in any mode; additionally drives LIMIT injection on read_only SELECTs. 0 disables both.")

	return cmd
}

// renderExecText emits the text-mode rendering of an ExecuteResult. The
// shape splits along the same axis as the JSON envelope:
//
//   - Result-set statements (SELECT, RETURNING) render as a tabwriter
//     table with column headers, followed by a "(N rows[, truncated])"
//     trailer that names the row count.
//
//   - Side-effect-only statements (INSERT/UPDATE/DELETE without
//     RETURNING, DDL) render as the cluster's command tag on a single
//     line ("INSERT 0 5", "CREATE TABLE", etc.), which matches what an
//     operator would see in psql / cockroach sql.
//
//   - LIMIT injection is surfaced as a trailing line so an agent
//     parsing the text output can flag that the result may be
//     incomplete relative to the original SQL.
func renderExecText(w io.Writer, res conn.ExecuteResult) error {
	if len(res.Columns) == 0 {
		if _, err := fmt.Fprintln(w, res.CommandTag); err != nil {
			return err
		}
		return writeLimitInjectedFooter(w, res.LimitInjected)
	}

	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	header := joinCells(columnNames(res.Columns))
	if _, err := fmt.Fprintln(tw, header); err != nil {
		return err
	}
	for _, row := range res.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = formatCell(v)
		}
		if _, err := fmt.Fprintln(tw, joinCells(cells)); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	trailer := fmt.Sprintf("(%d rows)", res.RowsReturned)
	if res.Truncated {
		trailer = fmt.Sprintf("(%d rows, truncated)", res.RowsReturned)
	}
	if _, err := fmt.Fprintln(w, trailer); err != nil {
		return err
	}
	return writeLimitInjectedFooter(w, res.LimitInjected)
}

// writeLimitInjectedFooter prints the "(LIMIT N injected)" trailer
// when the rewriter ran. Pulled out so both the tabular and the
// command-tag branches share the same wording.
func writeLimitInjectedFooter(w io.Writer, limit *int) error {
	if limit == nil {
		return nil
	}
	_, err := fmt.Fprintf(w, "(LIMIT %d injected)\n", *limit)
	return err
}

// columnNames extracts the Name field from each ColumnMeta in order,
// for tabwriter header rendering. Kept as a separate helper so the
// renderer body stays focused on flow control rather than slice
// manipulation.
func columnNames(cols []conn.ColumnMeta) []string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return names
}

// joinCells joins cells with the tab separator that tabwriter
// expects. strings.Join is enough — no per-cell escaping is needed
// because tabwriter only treats \t and \n specially.
func joinCells(cells []string) string {
	return strings.Join(cells, "\t")
}

// formatCell stringifies a single result-set value for tabwriter
// rendering. NULL is the conventional uppercase token; everything else
// goes through fmt's %v default so ints, strings, floats, and times
// take their natural representation. Byte slices render as quoted
// hex-escapes via %q to avoid corrupting the terminal.
func formatCell(v any) string {
	if v == nil {
		return "NULL"
	}
	if b, ok := v.([]byte); ok {
		return fmt.Sprintf("%q", b)
	}
	return fmt.Sprintf("%v", v)
}
