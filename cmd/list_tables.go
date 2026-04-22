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

  crdb-sql list-tables --schema schema.sql

If no --schema is supplied and a crdb-sql.yaml config is present in
the working directory (or pointed at by --config), list-tables expands
the config's schema globs across all sql pairs into one combined
catalog and lists tables from there. Explicit --schema flags win
outright (no merging with config-discovered files).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, baseEnv, err := newEnvelope(state, output.TierSchemaFile, cmd)
			if err != nil {
				return r.RenderError(baseEnv, err)
			}

			// Config fallback: when no explicit --schema is given but a
			// crdb-sql.yaml has been auto-discovered (or pointed at by
			// --config), expand every pair's schema globs into one
			// catalog. Explicit --schema flags continue to win outright.
			if len(schemaFiles) == 0 && state.cfg != nil {
				paths, err := expandConfigSchemaPaths(state.cfg)
				if err != nil {
					return r.RenderError(baseEnv, err)
				}
				if len(paths) == 0 {
					// Config was loaded but its globs matched no files.
					// Distinguish this from the no-config case so users
					// don't waste time wondering whether their config was
					// even discovered.
					return r.RenderError(baseEnv,
						fmt.Errorf("crdb-sql.yaml at %s has no schema files matching its globs; check the schema patterns or pass --schema explicitly",
							state.cfg.BaseDir))
				}
				cat, err := catalog.LoadFiles(paths)
				if err != nil {
					return renderSchemaLoadError(r, baseEnv, err)
				}
				return renderListTables(r, baseEnv, cat)
			}

			if len(schemaFiles) == 0 {
				return r.RenderError(baseEnv,
					fmt.Errorf("at least one --schema file is required (or a crdb-sql.yaml with matching schema globs)"))
			}

			cat, err := catalog.LoadFiles(schemaFiles)
			if err != nil {
				return renderSchemaLoadError(r, baseEnv, err)
			}

			return renderListTables(r, baseEnv, cat)
		},
	}

	cmd.Flags().StringArrayVar(&schemaFiles, "schema", nil,
		"schema SQL file(s) to load (repeatable)")

	return cmd
}

// renderListTables emits the table-name list for cat through the
// standard envelope. Schema-load warnings on cat are appended first so
// they ride along regardless of how many tables were found. Both the
// explicit --schema and config-fallback branches funnel through here
// so the two paths render identically.
func renderListTables(r output.Renderer, baseEnv output.Envelope, cat *catalog.Catalog) error {
	appendSchemaWarnings(&baseEnv, cat)

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
}
