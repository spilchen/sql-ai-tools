// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/spilchen/sql-ai-tools/internal/catalog"
	"github.com/spilchen/sql-ai-tools/internal/conn"
	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/output"
)

// listTablesResult is the JSON-serializable payload for the schema-file
// path of the list-tables command. It wraps the table names in a struct
// rather than marshalling a bare slice so the shape can be extended
// (e.g. with column counts) without breaking consumers.
type listTablesResult struct {
	Tables []string `json:"tables"`
}

// listTablesLiveResult is the JSON-serializable payload for the live
// (Tier 3) path of list-tables. It carries (schema, name) tuples so
// agents enumerating a multi-schema database can render qualified names
// without losing track of provenance. The shape diverges from
// listTablesResult deliberately: the envelope's `tier` field tells
// consumers which payload to expect (schema_file → []string, connected
// → []TableRef).
type listTablesLiveResult struct {
	Tables []conn.TableRef `json:"tables"`
}

// newListTablesCmd builds the `crdb-sql list-tables` subcommand. It
// resolves a table list from one of three escape hatches, in order:
//
//  1. Explicit --schema files (Tier 2, schema-file path).
//  2. A crdb-sql.yaml config file's schema globs (Tier 2).
//  3. A live cluster connection (--dsn or CRDB_DSN, Tier 3) — falls
//     back to information_schema introspection so users can list tables
//     against a fresh cluster without first dumping a schema file.
//
// When the live path is taken the output shape changes (see
// listTablesLiveResult); the envelope's `tier` field tells consumers
// which shape to expect.
func newListTablesCmd(state *rootState) *cobra.Command {
	var (
		schemaFiles   []string
		includeSystem bool
	)

	cmd := &cobra.Command{
		Use:   "list-tables",
		Short: "List tables from schema files or a live cluster",
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
outright (no merging with config-discovered files).

If neither --schema nor a config is available but --dsn (or CRDB_DSN)
points at a live cluster, list-tables falls back to querying
information_schema for tables in the DSN's database. By default,
only BASE TABLEs in non-system schemas are returned. Pass
--include-system to broaden the listing to every relation visible
in information_schema (BASE TABLEs, views, sequences, system
schemas) — useful for cluster spelunking but rarely what you want.
The text output qualifies each name as "schema.table" so
multi-schema listings stay unambiguous.`,
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

			// Live-cluster fallback: with no schema source available
			// but a DSN configured, introspect information_schema. This
			// is the Tier 3 path; switch the envelope's tier so
			// consumers can branch on it (the JSON payload shape also
			// differs — see listTablesLiveResult).
			if len(schemaFiles) == 0 && state.dsn != "" {
				return runListTablesLive(cmd.Context(), r, state.dsn, includeSystem, baseEnv)
			}

			if len(schemaFiles) == 0 {
				return r.RenderError(baseEnv,
					fmt.Errorf("at least one --schema file is required (or a crdb-sql.yaml with matching schema globs, or --dsn / CRDB_DSN to query a live cluster)"))
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
	cmd.Flags().BoolVar(&includeSystem, "include-system", false,
		"on the live-cluster path, return every relation visible in information_schema (system schemas, views, sequences); ignored on the schema-file path")

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

// runListTablesLive executes the Tier 3 live-cluster path: open a
// Manager, query information_schema, render the result. The envelope
// is upgraded to TierConnected (overwriting the TierSchemaFile that
// newEnvelope provides as the default for this command) and
// ConnectionStatus flips to ConnectionConnected only after a
// successful round-trip — a pre-flight error keeps the disconnected
// status so the envelope reflects what actually happened.
func runListTablesLive(
	ctx context.Context, r output.Renderer, dsn string, includeSystem bool, baseEnv output.Envelope,
) error {
	baseEnv.Tier = output.TierConnected

	mgr := conn.NewManager(dsn)
	defer mgr.Close(ctx) //nolint:errcheck // best-effort cleanup

	tables, err := mgr.ListTablesFromCluster(ctx, conn.ListOptions{IncludeSystem: includeSystem})
	if err != nil {
		baseEnv.Errors = append(baseEnv.Errors, diag.FromClusterError(err, ""))
		baseEnv.Data = nil
		if rerr := r.Render(baseEnv, func(w io.Writer) error {
			_, werr := io.WriteString(w, err.Error()+"\n")
			return werr
		}); rerr != nil {
			return rerr
		}
		return output.ErrRendered
	}

	baseEnv.ConnectionStatus = output.ConnectionConnected

	data, err := json.Marshal(listTablesLiveResult{Tables: tables})
	if err != nil {
		return r.RenderError(baseEnv, err)
	}
	baseEnv.Data = data

	return r.Render(baseEnv, func(w io.Writer) error {
		for _, t := range tables {
			if _, err := fmt.Fprintf(w, "%s.%s\n", t.Schema, t.Name); err != nil {
				return err
			}
		}
		return nil
	})
}
