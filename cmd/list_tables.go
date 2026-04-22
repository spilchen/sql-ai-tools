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

	"github.com/spilchen/sql-ai-tools/internal/catalog"
	"github.com/spilchen/sql-ai-tools/internal/output"
)

// listTablesResult is the JSON-serializable payload for the list-tables
// command. It wraps the table names in a struct rather than marshalling
// a bare slice so the shape can be extended (e.g. with column counts)
// without breaking consumers.
type listTablesResult struct {
	Tables []string `json:"tables"`
}

// newListTablesCmd builds the `crdb-sql list-tables` subcommand. It
// loads one or more schema files (--schema), parses them with the
// CockroachDB parser, and lists all table names found.
//
// This is a Tier 2 (schema-file) command: it works offline but
// requires at least one --schema file.
func newListTablesCmd(state *rootState) *cobra.Command {
	var schemaFiles []string

	cmd := &cobra.Command{
		Use:   "list-tables",
		Short: "List tables from schema files",
		Long: `Load CREATE TABLE definitions from one or more --schema files and
print the names of all tables found.

Schema files are plain SQL containing CREATE TABLE statements. You can
obtain one from a running CockroachDB cluster with:

  cockroach sql -e 'SHOW CREATE ALL TABLES' > schema.sql

Then list the tables:

  crdb-sql list-tables --schema schema.sql`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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

			tables := cat.TableNames()
			if tables == nil {
				tables = []string{}
			}

			data, err := json.Marshal(listTablesResult{Tables: tables})
			if err != nil {
				return r.RenderError(baseEnv, err)
			}
			baseEnv.Data = data

			return r.Render(baseEnv, func(w io.Writer) error {
				for _, name := range tables {
					if _, err := fmt.Fprintln(w, name); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}

	cmd.Flags().StringArrayVar(&schemaFiles, "schema", nil,
		"schema SQL file(s) to load (repeatable)")

	return cmd
}
