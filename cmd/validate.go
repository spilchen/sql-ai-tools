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
	"github.com/spilchen/sql-ai-tools/internal/semcheck"
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

			stmts, parseErr := parser.Parse(sql)
			if parseErr != nil {
				return renderParseError(r, baseEnv, parseErr, sql)
			}

			if typeErrs := semcheck.CheckExprTypes(stmts, sql); len(typeErrs) > 0 {
				return renderDiagErrors(r, baseEnv, typeErrs)
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
	return renderDiagErrors(r, env, []output.Error{diag.FromParseError(parseErr, sql)})
}

// renderDiagErrors renders one or more diagnostic errors (parse or
// type-check) through the standard envelope path.
func renderDiagErrors(r output.Renderer, env output.Envelope, errs []output.Error) error {
	env.Errors = errs
	env.Data = nil

	if err := r.Render(env, func(w io.Writer) error {
		for _, diagErr := range errs {
			pos := diagErr.Position
			if pos != nil {
				if _, werr := fmt.Fprintf(w, "%d:%d: %s [%s]\n", pos.Line, pos.Column, diagErr.Message, diagErr.Code); werr != nil {
					return werr
				}
			} else {
				if _, werr := fmt.Fprintf(w, "%s [%s]\n", diagErr.Message, diagErr.Code); werr != nil {
					return werr
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return output.ErrRendered
}
