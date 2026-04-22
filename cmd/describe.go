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
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/spilchen/sql-ai-tools/internal/catalog"
	"github.com/spilchen/sql-ai-tools/internal/output"
)

// newDescribeCmd builds the `crdb-sql describe` subcommand. It loads
// one or more schema files (--schema), parses them with the
// CockroachDB parser, and describes the named table's columns, primary
// key, and indexes.
//
// This is a Tier 2 (schema-file) command: it works offline but
// requires at least one --schema file.
func newDescribeCmd(state *rootState) *cobra.Command {
	var schemaFiles []string

	cmd := &cobra.Command{
		Use:   "describe TABLE",
		Short: "Describe a table from schema files",
		Long: `Load CREATE TABLE definitions from one or more --schema files and
print the named table's columns, primary key, and indexes.

Schema files are plain SQL containing CREATE TABLE statements. You can
obtain one from a running CockroachDB cluster with:

  cockroach sql -e 'SHOW CREATE ALL TABLES' > schema.sql

Then describe any table:

  crdb-sql describe users --schema schema.sql`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, baseEnv, err := newEnvelope(state, output.TierSchemaFile, cmd)
			if err != nil {
				return r.RenderError(baseEnv, err)
			}

			if len(schemaFiles) == 0 {
				return r.RenderError(baseEnv,
					fmt.Errorf("at least one --schema file is required"))
			}

			cat, err := catalog.LoadFiles(schemaFiles)
			if err != nil {
				return r.RenderError(baseEnv, err)
			}

			for _, w := range cat.Warnings() {
				baseEnv.Errors = append(baseEnv.Errors, output.Error{
					Code:     "schema_warning",
					Severity: output.SeverityWarning,
					Message:  w,
				})
			}

			tableName := args[0]
			tbl, ok := cat.Table(tableName)
			if !ok {
				available := cat.TableNames()
				if len(available) == 0 {
					return r.RenderError(baseEnv,
						fmt.Errorf("table %q not found; schema files contain no tables", tableName))
				}
				return r.RenderError(baseEnv,
					fmt.Errorf("table %q not found; available tables: %s",
						tableName, strings.Join(available, ", ")))
			}

			data, err := json.Marshal(tbl)
			if err != nil {
				return r.RenderError(baseEnv, err)
			}
			baseEnv.Data = data

			return r.Render(baseEnv, func(w io.Writer) error {
				return renderTableText(w, tbl)
			})
		},
	}

	cmd.Flags().StringArrayVar(&schemaFiles, "schema", nil,
		"schema SQL file(s) to load (repeatable)")

	return cmd
}

func renderTableText(w io.Writer, tbl catalog.Table) error {
	if _, err := fmt.Fprintf(w, "Table: %s\n\nColumns:\n", tbl.Name); err != nil {
		return err
	}

	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "  NAME\tTYPE\tNULLABLE\tDEFAULT"); err != nil {
		return err
	}
	for _, col := range tbl.Columns {
		def := "-"
		if col.Default != nil {
			def = *col.Default
		}
		if _, err := fmt.Fprintf(tw, "  %s\t%s\t%t\t%s\n",
			col.Name, col.Type, col.Nullable, def); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if len(tbl.PrimaryKey) > 0 {
		if _, err := fmt.Fprintf(w, "\nPrimary Key: %s\n",
			strings.Join(tbl.PrimaryKey, ", ")); err != nil {
			return err
		}
	}

	if len(tbl.Indexes) > 0 {
		if _, err := fmt.Fprintln(w, "\nIndexes:"); err != nil {
			return err
		}
		tw = tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(tw, "  NAME\tCOLUMNS\tUNIQUE"); err != nil {
			return err
		}
		for _, idx := range tbl.Indexes {
			if _, err := fmt.Fprintf(tw, "  %s\t%s\t%t\n",
				idx.Name, strings.Join(idx.Columns, ", "), idx.Unique); err != nil {
				return err
			}
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	return nil
}
