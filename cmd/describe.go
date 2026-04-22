// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/spilchen/sql-ai-tools/internal/catalog"
	"github.com/spilchen/sql-ai-tools/internal/config"
	"github.com/spilchen/sql-ai-tools/internal/output"
)

const stdinSentinel = "-"

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

  crdb-sql describe users --schema schema.sql

Use --schema - to read DDL from stdin, allowing direct piping:

  cockroach sql -e 'SHOW CREATE ALL TABLES' | crdb-sql describe users --schema -

If no --schema is supplied and a crdb-sql.yaml config is present in
the working directory (or pointed at by --config), describe expands
the config's schema globs across all sql pairs into one combined
catalog and looks up the table there. Explicit --schema flags win
outright (no merging with config-discovered files).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, baseEnv, err := newEnvelope(state, output.TierSchemaFile, cmd)
			if err != nil {
				return r.RenderError(baseEnv, err)
			}

			tableName := args[0]

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
				return describeTableFromCatalog(r, baseEnv, cat, tableName)
			}

			if len(schemaFiles) == 0 {
				return r.RenderError(baseEnv,
					fmt.Errorf("at least one --schema file is required (or a crdb-sql.yaml with matching schema globs)"))
			}

			sources, err := buildSchemaSources(schemaFiles, cmd.InOrStdin())
			if err != nil {
				return r.RenderError(baseEnv, err)
			}

			cat, err := catalog.Load(sources)
			if err != nil {
				return renderSchemaLoadError(r, baseEnv, err)
			}

			return describeTableFromCatalog(r, baseEnv, cat, tableName)
		},
	}

	cmd.Flags().StringArrayVar(&schemaFiles, "schema", nil,
		`schema SQL file(s) to load (repeatable); use "-" for stdin`)

	return cmd
}

// buildSchemaSources converts the raw --schema flag values into
// catalog.SchemaSource entries. The sentinel "-" means "read from
// stdin"; it may appear at most once.
func buildSchemaSources(flags []string, stdin io.Reader) ([]catalog.SchemaSource, error) {
	var sources []catalog.SchemaSource
	stdinUsed := false

	for _, f := range flags {
		if f != stdinSentinel {
			sources = append(sources, catalog.SchemaSource{Path: f})
			continue
		}
		if stdinUsed {
			return nil, fmt.Errorf("--schema - (stdin) can only be specified once")
		}
		stdinUsed = true

		data, err := io.ReadAll(io.LimitReader(stdin, catalog.MaxSchemaFileSize+1))
		if err != nil {
			return nil, fmt.Errorf("reading stdin: %w", err)
		}
		if int64(len(data)) > catalog.MaxSchemaFileSize {
			return nil, fmt.Errorf("stdin input is too large (%d bytes, max %d)",
				len(data), catalog.MaxSchemaFileSize)
		}
		sources = append(sources, catalog.SchemaSource{
			SQL:   string(data),
			Label: "stdin",
		})
	}
	return sources, nil
}

// describeTableFromCatalog renders one table from cat through the
// standard envelope. Schema-load warnings on cat are appended to the
// envelope first so they ride along regardless of lookup outcome.
// "Not found" errors include the available table list to nudge users
// toward the right name (e.g. case or pluralization typos).
func describeTableFromCatalog(
	r output.Renderer, baseEnv output.Envelope, cat *catalog.Catalog, tableName string,
) error {
	appendSchemaWarnings(&baseEnv, cat)

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
}

// expandConfigSchemaPaths flattens every SQLPair's schema globs into
// one sorted, deduplicated path list. Each pair's ExpandSchema already
// dedupes within itself; the outer map handles overlap across pairs
// (e.g. two pairs both matching schema/shared/*.sql). The final
// sort.Strings is needed because concatenation across pairs breaks
// the per-pair ordering that ExpandSchema guarantees individually.
func expandConfigSchemaPaths(cfg *config.File) ([]string, error) {
	seen := make(map[string]struct{})
	var paths []string
	for _, pair := range cfg.SQL {
		expanded, err := pair.ExpandSchema(cfg.BaseDir)
		if err != nil {
			return nil, err
		}
		for _, p := range expanded {
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths, nil
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
