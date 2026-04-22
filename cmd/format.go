// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/pgwire/pgerror"
	"github.com/spf13/cobra"

	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/sqlformat"
	"github.com/spilchen/sql-ai-tools/internal/sqlinput"
)

// colorMode is the value space accepted by --color. It mirrors the
// convention used by git, ls, ripgrep, and similar tools.
//
//	auto   — colorize when stdout is a terminal and --output is text
//	always — colorize regardless of stdout type (text mode only)
//	never  — never colorize
//
// JSON output never receives ANSI escapes regardless of mode, because
// agent consumers must be able to round-trip the envelope through JSON
// parsers without re-encoding pitfalls.
type colorMode string

const (
	colorAuto   colorMode = "auto"
	colorAlways colorMode = "always"
	colorNever  colorMode = "never"
)

func parseColorMode(s string) (colorMode, error) {
	switch colorMode(s) {
	case colorAuto, colorAlways, colorNever:
		return colorMode(s), nil
	default:
		return "", fmt.Errorf("invalid --color %q: valid choices are %q, %q, %q",
			s, colorAuto, colorAlways, colorNever)
	}
}

// isTerminal reports whether w is a character device — the standard
// proxy for "user is looking at this in a terminal". Returns false for
// any writer that is not an *os.File (e.g. a bytes.Buffer in tests, or
// a pipe), which is the safe default for --color=auto.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// newFormatCmd builds the `crdb-sql format` subcommand. It reads SQL
// from the -e flag, a positional file argument, or stdin, parses it
// with the CockroachDB parser, and re-emits it in canonical
// pretty-printed form using tree.DefaultPrettyCfg.
//
// This is a Tier 1 (zero-config) command: it works offline with no
// schema files or cluster connection.
func newFormatCmd(state *rootState) *cobra.Command {
	var (
		expr      string
		colorFlag string
	)

	cmd := &cobra.Command{
		Use:   "format [file]",
		Short: "Pretty-print SQL statements",
		Long: `Parse SQL and re-emit it in canonical pretty-printed form using the
CockroachDB parser's built-in formatter. Input is read from the -e flag
(inline SQL), a positional file argument, or stdin.

Pasted output from a cockroach sql REPL session is auto-cleaned: primary
prompts (user@host:port/db>) and continuation prompts (->) are stripped
before parsing, so transcripts paste in unmodified.

Use --color=always|never|auto to control ANSI syntax highlighting in
text mode. The default (auto) colorizes only when stdout is a terminal.
JSON output is never colorized.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, baseEnv, err := newEnvelope(state, output.TierZeroConfig, cmd)
			if err != nil {
				return r.RenderError(baseEnv, err)
			}

			mode, err := parseColorMode(colorFlag)
			if err != nil {
				return r.RenderError(baseEnv, err)
			}

			sql, err := sqlinput.ReadSQL(expr, args, cmd.InOrStdin())
			if err != nil {
				return r.RenderError(baseEnv, err)
			}

			sql = strings.TrimSpace(sqlformat.StripShellPrompts(sql))

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

			// JSON payload is always uncolored. Text path opt-in by
			// mode + TTY. The decision happens here (not inside the
			// textFn closure) so it is visible to the reader.
			textOut := formatted
			useColor := mode == colorAlways ||
				(mode == colorAuto && isTerminal(cmd.OutOrStdout()))
			if useColor {
				textOut = sqlformat.Highlight(formatted)
			}

			data, err := json.Marshal(struct {
				FormattedSQL string `json:"formatted_sql"`
			}{FormattedSQL: formatted})
			if err != nil {
				return r.RenderError(baseEnv, err)
			}
			baseEnv.Data = data

			return r.Render(baseEnv, func(w io.Writer) error {
				_, werr := fmt.Fprintln(w, textOut)
				return werr
			})
		},
	}

	cmd.Flags().StringVarP(&expr, "expression", "e", "", "inline SQL to format")
	cmd.Flags().StringVar(&colorFlag, "color", string(colorAuto),
		"ANSI syntax highlighting in text output: auto|always|never")

	return cmd
}
