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
	"github.com/spilchen/sql-ai-tools/internal/risk"
	"github.com/spilchen/sql-ai-tools/internal/sqlinput"
	"github.com/spilchen/sql-ai-tools/internal/version"
)

// newRiskCmd builds the `crdb-sql risk` subcommand. It reads SQL from
// the -e flag, a positional file argument, or stdin, parses it with
// the CockroachDB parser, and runs the default set of AST-only risk
// rules against each statement. Findings are emitted as structured
// output with reason codes, severity, and fix hints.
//
// This is a Tier 1 (zero-config) command: it works offline with no
// schema files or cluster connection.
func newRiskCmd(state *rootState) *cobra.Command {
	var expr string

	cmd := &cobra.Command{
		Use:   "risk [file]",
		Short: "Detect risky SQL patterns",
		Long: `Analyze SQL for dangerous patterns such as DELETE or UPDATE without a
WHERE clause, DROP/TRUNCATE statements, SELECT *, SERIAL or missing
primary keys, deep OFFSET pagination, and XA two-phase-commit
statements. Input is read from the -e flag (inline SQL), a positional
file argument, or stdin. Each finding includes a reason code, severity,
human-readable message, and fix hint.

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

			originalSQL := sql
			strip := preprocessSQL(&baseEnv, sql)
			sql = strip.Stripped

			parsed, parseErr := parser.Parse(sql)
			if parseErr != nil {
				return renderParseErrorTranslated(r, baseEnv, parseErr, sql, originalSQL, strip)
			}
			baseEnv.Errors = append(baseEnv.Errors,
				version.Inspect(parsed, state.targetVersion, nil)...)
			findings := risk.AnalyzeParsed(parsed, sql)

			data, err := json.Marshal(findings)
			if err != nil {
				return r.RenderError(baseEnv, err)
			}
			baseEnv.Data = data

			return r.Render(baseEnv, func(w io.Writer) error {
				for _, f := range findings {
					if _, werr := fmt.Fprintf(w, "%s\t%s\t%s\n", strings.ToUpper(string(f.Severity)), f.ReasonCode, f.Message); werr != nil {
						return werr
					}
					if _, werr := fmt.Fprintf(w, "  hint: %s\n", f.FixHint); werr != nil {
						return werr
					}
				}
				return nil
			})
		},
	}

	cmd.Flags().StringVarP(&expr, "expression", "e", "", "inline SQL to analyze")

	return cmd
}
