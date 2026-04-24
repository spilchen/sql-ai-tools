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

// newExplainDDLCmd builds the `crdb-sql explain-ddl` subcommand. It
// runs `EXPLAIN (DDL, SHAPE) <stmt>` against the cluster identified by
// --dsn (or the CRDB_DSN env var) and renders the resulting declarative
// schema-changer plan.
//
// Input modes mirror explain: -e for inline SQL, a positional file
// argument, or stdin. Output modes mirror explain too: text (the raw
// SHAPE output, exactly as `cockroach sql` would render it) and json
// (structured envelope with statement, operations, and raw_text).
//
// Like explain, this is a Tier 3 (connected) command. EXPLAIN (DDL,
// SHAPE) does not execute the wrapped DDL — it only asks the
// declarative schema changer to compile a plan — and the safety
// allowlist still gates the call: in the default read_only mode every
// DDL is rejected (since DDL modifies schema), so users must escalate
// to --mode=safe_write or --mode=full_access to exercise this
// command. safe_write admits DDL but still rejects nested EXPLAIN,
// privilege/role changes (DCL), cluster admin, tenant management,
// and non-DDL inputs; full_access admits any DDL that parses while
// still rejecting nested EXPLAIN and non-DDL inputs.
func newExplainDDLCmd(state *rootState) *cobra.Command {
	var (
		expr    string
		mode    string
		timeout time.Duration
	)

	cmd := &cobra.Command{
		Use:   "explain-ddl [file]",
		Short: "Run EXPLAIN (DDL, SHAPE) against the cluster and return the schema-change plan as structured JSON",
		Long: `Connect to a CockroachDB cluster and run EXPLAIN (DDL, SHAPE)
against the supplied DDL statement. The wrapped statement is not executed
— the declarative schema changer only compiles a plan. Input is read
from the -e flag (inline SQL), a positional file argument, or stdin. The
connection string is read from --dsn or CRDB_DSN (flag wins).

The --mode flag selects the safety policy applied before the cluster
call. The default "read_only" rejects every DDL (since DDL modifies
schema), so this command requires --mode=safe_write or
--mode=full_access. safe_write admits DDL but still rejects nested
EXPLAIN, privilege/role changes (DCL), cluster administration,
tenant management, and non-DDL inputs; full_access admits any DDL
that parses while still rejecting nested EXPLAIN and non-DDL
inputs.`,
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

			if state.dsn == "" {
				return r.RenderError(baseEnv,
					fmt.Errorf("no connection string: pass --dsn or set CRDB_DSN"))
			}

			parsedMode, err := safety.ParseMode(mode)
			if err != nil {
				return r.RenderError(baseEnv, err)
			}

			violation, err := safety.Check(parsedMode, safety.OpExplainDDL, sql)
			if err != nil {
				return r.RenderErrorEntry(baseEnv, err, diag.FromParseError(err, sql))
			}
			if violation != nil {
				return r.RenderErrorEntry(baseEnv,
					fmt.Errorf("safety violation: %s", violation.Reason),
					safety.Envelope(violation))
			}

			mgr := conn.NewManager(state.dsn, conn.WithStatementTimeout(timeout))
			defer mgr.Close(cmd.Context()) //nolint:errcheck // best-effort cleanup

			result, err := mgr.ExplainDDL(cmd.Context(), sql)
			if err != nil {
				return r.RenderErrorEntry(baseEnv, err, diag.FromClusterError(err, sql))
			}

			baseEnv.ConnectionStatus = output.ConnectionConnected

			data, err := json.Marshal(result)
			if err != nil {
				return r.RenderError(baseEnv, err)
			}
			baseEnv.Data = data

			return r.Render(baseEnv, func(w io.Writer) error {
				_, werr := io.WriteString(w, result.RawText)
				return werr
			})
		},
	}

	cmd.Flags().StringVarP(&expr, "expression", "e", "", "inline DDL to explain")
	cmd.Flags().StringVar(&mode, "mode", string(safety.DefaultMode),
		"safety mode: read_only (default), safe_write, full_access")
	cmd.Flags().DurationVar(&timeout, "timeout", conn.DefaultStatementTimeout,
		"statement timeout for the wrapped EXPLAIN (DDL, SHAPE) call")

	return cmd
}
