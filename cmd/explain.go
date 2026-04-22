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

	"github.com/spf13/cobra"

	"github.com/spilchen/sql-ai-tools/internal/conn"
	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/output"
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
// This is a Tier 3 (connected) command. Read-only safety enforcement
// (statement allowlist, transaction wrapping) is deferred to issue #21.
func newExplainCmd(state *rootState) *cobra.Command {
	var expr string

	cmd := &cobra.Command{
		Use:   "explain [file]",
		Short: "Run EXPLAIN against the cluster and return the plan as structured JSON",
		Long: `Connect to a CockroachDB cluster and run EXPLAIN against the supplied
DML statement. The wrapped statement is not executed. Input is read from
the -e flag (inline SQL), a positional file argument, or stdin. The
connection string is read from --dsn or CRDB_DSN (flag wins).`,
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

			mgr := conn.NewManager(state.dsn)
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

	return cmd
}
