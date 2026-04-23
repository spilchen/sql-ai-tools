// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/spf13/cobra"

	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/sqlinput"
	"github.com/spilchen/sql-ai-tools/internal/summarize"
	"github.com/spilchen/sql-ai-tools/internal/version"
)

// newSummarizeCmd builds the `crdb-sql summarize` subcommand. It reads
// SQL from the -e flag, a positional file argument, or stdin, parses
// it with the CockroachDB parser, and emits a per-statement structured
// summary: operation, tables, predicates, joins, mutated columns, and
// a delegated risk level.
//
// This is a Tier 1 (zero-config) command: it works offline with no
// schema files or cluster connection.
func newSummarizeCmd(state *rootState) *cobra.Command {
	var expr string

	cmd := &cobra.Command{
		Use:   "summarize [file]",
		Short: "Structured summary of SQL statements",
		Long: `Produce a structured per-statement summary of SQL: operation
(SELECT/INSERT/UPDATE/DELETE/UPSERT/OTHER), tables touched, top-level
WHERE predicates, joins, columns mutated by DML (affected_columns),
the full read-and-write column footprint (referenced_columns), a
select_star flag set when the projection uses '*' or 't.*', and a
risk level delegated to the same rules as 'crdb-sql risk'. Input is
read from the -e flag, a positional file argument, or stdin.`,
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

			parsed, parseErr := parser.Parse(sql)
			if parseErr != nil {
				return renderParseError(r, baseEnv, parseErr, sql)
			}
			baseEnv.Errors = append(baseEnv.Errors,
				version.Inspect(parsed, state.targetVersion, nil)...)
			summaries := summarize.Parsed(parsed, sql)

			data, err := json.Marshal(summaries)
			if err != nil {
				return r.RenderError(baseEnv, err)
			}
			baseEnv.Data = data

			return r.Render(baseEnv, func(w io.Writer) error {
				return renderSummariesText(w, summaries)
			})
		},
	}

	cmd.Flags().StringVarP(&expr, "expression", "e", "", "inline SQL to summarize")

	return cmd
}

// renderSummariesText prints one labeled block per statement. A blank
// line separates blocks so multi-statement output is easy to scan.
// Empty fields are rendered as "(none)" rather than blank so a quick
// visual scan distinguishes "no joins" from "joins were not analyzed".
func renderSummariesText(w io.Writer, summaries []summarize.Summary) error {
	for i, s := range summaries {
		if i > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		if err := renderSummaryText(w, s); err != nil {
			return err
		}
	}
	return nil
}

func renderSummaryText(w io.Writer, s summarize.Summary) error {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	rows := []struct {
		label string
		value string
	}{
		{"operation", string(s.Operation)},
		{"tag", s.Tag},
		{"tables", joinOrNone(s.Tables)},
		{"joins", joinSummaryStrings(s.Joins)},
		{"predicates", joinOrNone(s.Predicates)},
		{"affected_columns", joinOrNone(s.AffectedColumns)},
		{"referenced_columns", joinOrNone(s.ReferencedColumns)},
		{"select_star", strconv.FormatBool(s.SelectStar)},
		{"risk", string(s.RiskLevel)},
	}
	for _, row := range rows {
		if _, err := fmt.Fprintf(tw, "%s:\t%s\n", row.label, row.value); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func joinOrNone(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}
	return strings.Join(items, ", ")
}

// joinSummaryStrings flattens the structured Join entries into a
// single comma-separated line for the text view; the JSON output
// preserves the structured form for agents that want to consume it.
func joinSummaryStrings(joins []summarize.Join) string {
	if len(joins) == 0 {
		return "(none)"
	}
	parts := make([]string, len(joins))
	for i, j := range joins {
		left := j.Left
		if left == "" {
			left = "?"
		}
		right := j.Right
		if right == "" {
			right = "?"
		}
		if j.Condition == "" {
			parts[i] = fmt.Sprintf("%s %s %s", j.Type, left, right)
			continue
		}
		parts[i] = fmt.Sprintf("%s %s %s ON %s", j.Type, left, right, j.Condition)
	}
	return strings.Join(parts, "; ")
}
