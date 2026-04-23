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

// newExplainCmd builds the `crdb-sql explain` subcommand. It runs
// `EXPLAIN <stmt>` against the cluster identified by --dsn (or the
// CRDB_DSN env var) and renders the resulting plan tree.
//
// Input modes mirror format/parse/validate: -e for inline SQL, a
// positional file argument, or stdin. Output modes mirror ping: text
// (the raw EXPLAIN tabular rows) and json (structured envelope with
// header, plan tree, and raw rows).
//
// This is a Tier 3 (connected) command. The --mode flag (default
// read_only) and --timeout flag (default 30s) gate the cluster call:
// the safety allowlist (internal/safety) rejects DML/DDL inner
// statements before any pgwire round-trip, and the read-only
// transaction wrapper inside conn.Manager applies the statement
// timeout for defense-in-depth.
func newExplainCmd(state *rootState) *cobra.Command {
	var (
		expr    string
		mode    string
		timeout time.Duration
	)

	cmd := &cobra.Command{
		Use:   "explain [file]",
		Short: "Run EXPLAIN against the cluster and return the plan as structured JSON",
		Long: `Connect to a CockroachDB cluster and run EXPLAIN against the supplied
DML statement. The wrapped statement is not executed. Input is read from
the -e flag (inline SQL), a positional file argument, or stdin. The
connection string is read from --dsn or CRDB_DSN (flag wins).

The --mode flag selects the safety policy applied before the cluster
call. Default is "read_only", which permits SELECT, SHOW, and other
non-mutating statements. "safe_write" and "full_access" are reserved
for follow-up issues and currently reject every statement.`,
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

			// Safety check runs before any cluster contact: a
			// rejection produces a structured envelope with
			// connection_status=disconnected, exactly as if --dsn
			// were missing.
			violation, err := safety.Check(parsedMode, safety.OpExplain, sql)
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

			result, err := mgr.Explain(cmd.Context(), sql)
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
				for _, row := range result.RawRows {
					if _, werr := fmt.Fprintln(w, row); werr != nil {
						return werr
					}
				}
				return nil
			})
		},
	}

	cmd.Flags().StringVarP(&expr, "expression", "e", "", "inline SQL to explain")
	cmd.Flags().StringVar(&mode, "mode", string(safety.DefaultMode),
		"safety mode: read_only (default), safe_write, full_access")
	cmd.Flags().DurationVar(&timeout, "timeout", conn.DefaultStatementTimeout,
		"statement timeout for the wrapped EXPLAIN call")

	return cmd
}
