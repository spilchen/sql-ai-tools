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

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/spf13/cobra"

	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/sqlinput"
)

// newValidateCmd builds the `crdb-sql validate` subcommand. It reads
// SQL from the -e flag, a positional file argument, or stdin, parses
// it with the CockroachDB parser, and reports whether the SQL is
// syntactically valid. On parse failure the error is surfaced as a
// structured envelope entry with SQLSTATE code, severity, message,
// and source position.
//
// This is a Tier 1 (zero-config) command: it works offline with no
// schema files or cluster connection.
func newValidateCmd(state *rootState) *cobra.Command {
	var expr string

	cmd := &cobra.Command{
		Use:   "validate [file]",
		Short: "Check SQL for syntax errors",
		Long: `Parse SQL and report whether it is syntactically valid. On failure,
the error includes the SQLSTATE code, severity, message, and source
position (line/column/byte offset). Input is read from the -e flag
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

			// Trim surrounding whitespace so trailing newlines from
			// stdin/file do not skew position reporting.
			sql = strings.TrimSpace(sql)

			if _, parseErr := parser.Parse(sql); parseErr != nil {
				return renderParseError(r, baseEnv, parseErr, sql)
			}

			data, err := json.Marshal(struct {
				Valid bool `json:"valid"`
			}{Valid: true})
			if err != nil {
				return r.RenderError(baseEnv, err)
			}
			baseEnv.Data = data

			return r.Render(baseEnv, func(w io.Writer) error {
				_, werr := fmt.Fprintln(w, "Valid.")
				return werr
			})
		},
	}

	cmd.Flags().StringVarP(&expr, "expression", "e", "", "inline SQL to validate")

	return cmd
}

// renderParseError enriches a parser error with SQLSTATE code and
// position, then renders it through the standard envelope path. It
// bypasses Renderer.RenderError (which uses a generic
// "internal_error" code) so that agents receive the real SQLSTATE.
func renderParseError(r output.Renderer, env output.Envelope, parseErr error, sql string) error {
	diagErr := diag.FromParseError(parseErr, sql)
	env.Errors = []output.Error{diagErr}
	env.Data = nil

	if err := r.Render(env, func(w io.Writer) error {
		pos := diagErr.Position
		if pos != nil {
			_, werr := fmt.Fprintf(w, "%d:%d: %s [%s]\n", pos.Line, pos.Column, diagErr.Message, diagErr.Code)
			return werr
		}
		_, werr := fmt.Fprintf(w, "%s [%s]\n", diagErr.Message, diagErr.Code)
		return werr
	}); err != nil {
		return err
	}
	return output.ErrRendered
}
