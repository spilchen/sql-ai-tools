// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/spilchen/sql-ai-tools/internal/conn"
	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/output"
)

// newPingCmd builds the `crdb-sql ping` subcommand. It connects to a
// CockroachDB cluster via the DSN resolved by the root command (--dsn
// flag or CRDB_DSN env var) and returns the cluster ID and version.
//
// This is a Tier 3 (connected) command: it requires a live cluster.
func newPingCmd(state *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "ping",
		Short: "Test cluster connectivity and return cluster info",
		Long: `Connect to a CockroachDB cluster and return its cluster ID and
version. The connection string is read from the --dsn flag or the
CRDB_DSN environment variable (flag takes precedence).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, baseEnv, err := newEnvelope(state, output.TierConnected, cmd)
			if err != nil {
				return r.RenderError(baseEnv, err)
			}

			if state.dsn == "" {
				return r.RenderError(baseEnv,
					fmt.Errorf("no connection string: pass --dsn or set CRDB_DSN"))
			}

			mgr := conn.NewManager(state.dsn)
			defer mgr.Close(cmd.Context()) //nolint:errcheck

			info, err := mgr.Ping(cmd.Context())
			if err != nil {
				return r.RenderErrorEntry(baseEnv, err, diag.FromClusterError(err, ""))
			}

			baseEnv.ConnectionStatus = output.ConnectionConnected

			data, err := json.Marshal(info)
			if err != nil {
				return r.RenderError(baseEnv, err)
			}
			baseEnv.Data = data

			return r.Render(baseEnv, func(w io.Writer) error {
				_, werr := fmt.Fprintf(w,
					"cluster_id: %s\nversion: %s\nconnection_status: connected\n",
					info.ClusterID, info.Version)
				return werr
			})
		},
	}
}
