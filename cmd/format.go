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

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/pgwire/pgerror"
	"github.com/spf13/cobra"

	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/sqlformat"
	"github.com/spilchen/sql-ai-tools/internal/sqlinput"
)

// newFormatCmd builds the `crdb-sql format` subcommand. It reads SQL
// from the -e flag, a positional file argument, or stdin, parses it
// with the CockroachDB parser, and re-emits it in canonical
// pretty-printed form using tree.DefaultPrettyCfg.
//
// This is a Tier 1 (zero-config) command: it works offline with no
// schema files or cluster connection.
func newFormatCmd(state *rootState) *cobra.Command {
	var expr string

	cmd := &cobra.Command{
		Use:   "format [file]",
		Short: "Pretty-print SQL statements",
		Long: `Parse SQL and re-emit it in canonical pretty-printed form using the
CockroachDB parser's built-in formatter. Input is read from the -e flag
(inline SQL), a positional file argument, or stdin.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r := output.Renderer{Format: state.outputFormat, Out: cmd.OutOrStdout()}
			baseEnv := output.Envelope{
				Tier:             output.TierZeroConfig,
				ConnectionStatus: output.ConnectionDisconnected,
			}

			parserVer, err := parserVersion(Version)
			if err != nil {
				return r.RenderError(baseEnv, err)
			}
			baseEnv.ParserVersion = parserVer

			sql, err := sqlinput.ReadSQL(expr, args, cmd.InOrStdin())
			if err != nil {
				return r.RenderError(baseEnv, err)
			}

			sql = strings.TrimSpace(sql)

			formatted, err := sqlformat.Format(sql)
			if err != nil {
				// Format can fail during parsing (candidate PGCODE
				// present) or during pretty-printing (no candidate
				// code). Only parser errors get the enriched path.
				if pgerror.HasCandidateCode(err) {
					return renderParseError(r, baseEnv, err, sql)
				}
				return r.RenderError(baseEnv, err)
			}

			data, err := json.Marshal(struct {
				FormattedSQL string `json:"formatted_sql"`
			}{FormattedSQL: formatted})
			if err != nil {
				return r.RenderError(baseEnv, err)
			}
			baseEnv.Data = data

			return r.Render(baseEnv, func(w io.Writer) error {
				_, werr := fmt.Fprintln(w, formatted)
				return werr
			})
		},
	}

	cmd.Flags().StringVarP(&expr, "expression", "e", "", "inline SQL to format")

	return cmd
}
