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

	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/sqlinput"
	"github.com/spilchen/sql-ai-tools/internal/sqlparse"
	"github.com/spilchen/sql-ai-tools/internal/version"
)

// newParseCmd builds the `crdb-sql parse` subcommand. It reads SQL from
// the -e flag, a positional file argument, or stdin, parses it with the
// CockroachDB parser, and emits a per-statement classification
// containing the statement type (DDL/DML/DCL/TCL), the statement tag
// (e.g. "SELECT", "ALTER TABLE"), the original SQL text, and a
// normalized form with literal constants replaced by placeholders.
//
// This is a Tier 1 (zero-config) command: it works offline with no
// schema files or cluster connection.
func newParseCmd(state *rootState) *cobra.Command {
	var expr string

	cmd := &cobra.Command{
		Use:   "parse [file]",
		Short: "Classify SQL statements",
		Long: `Parse SQL and classify each statement. Input is read from the -e flag
(inline SQL), a positional file argument, or stdin. For each statement
the output includes the statement type (DDL, DML, DCL, or TCL), the
statement tag (e.g. SELECT, ALTER TABLE), the original SQL text, and a
normalized form with literal constants replaced by placeholders.

Pasted output from a cockroach sql REPL session is auto-cleaned: primary
prompts (user@host:port/db>) and continuation prompts (->) are stripped
before parsing, and the JSON envelope carries an input_preprocessed
warning so the modification is visible.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, baseEnv, err := newEnvelope(state, output.TierZeroConfig, cmd)
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

			parsed, parseErr := parser.Parse(sql)
			if parseErr != nil {
				return renderParseErrorTranslated(r, baseEnv, parseErr, sql, originalSQL, strip)
			}
			stmts := sqlparse.ClassifyParsed(parsed)
			baseEnv.Errors = append(baseEnv.Errors,
				version.Inspect(parsed, state.targetVersion, nil)...)

			data, err := json.Marshal(stmts)
			if err != nil {
				return r.RenderError(baseEnv, err)
			}
			baseEnv.Data = data

			return r.Render(baseEnv, func(w io.Writer) error {
				for _, s := range stmts {
					if _, werr := fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.StatementType, s.Tag, s.SQL, s.Normalized); werr != nil {
						return werr
					}
				}
				return nil
			})
		},
	}

	cmd.Flags().StringVarP(&expr, "expression", "e", "", "inline SQL to parse")

	return cmd
}
