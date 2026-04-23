// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

//go:build integration

// Integration tests for `crdb-sql list-tables` exercising the live-DSN
// fallback (Tier 3) against a real CockroachDB cluster. Build-tagged so
// `make test` stays fast; run via `make test-integration`. The shared
// cluster is provided by the cockroachtest harness; per-test databases
// keep these tests independent from any other suite that might run
// against the same cluster.

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/catalog"
	"github.com/spilchen/sql-ai-tools/internal/conn"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/testutil/cockroachtest"
)

// TestIntegrationListTablesLiveText covers the happy text-mode path:
// without --schema, the command falls back to information_schema and
// emits "schema.table" qualified names so multi-schema listings stay
// unambiguous.
func TestIntegrationListTablesLiveText(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	t.Setenv("CRDB_DSN", "")

	dbName := uniqueIntegDB(t, "lt_text")
	dropDB := setupIntegDB(t, cluster.DSN, dbName,
		"CREATE SCHEMA app",
		"CREATE TABLE public.users (id INT8 PRIMARY KEY)",
		"CREATE TABLE app.orders (id INT8 PRIMARY KEY)",
	)
	t.Cleanup(dropDB)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"list-tables", "--dsn", dsnFor(t, cluster.DSN, dbName)})

	require.NoError(t, root.Execute())
	require.Equal(t, "app.orders\npublic.users\n", stdout.String())
}

// TestIntegrationListTablesLiveJSON covers the structured envelope:
// tier flips to connected, connection_status is connected, and the
// payload uses the TableRef shape (not bare strings, which is the
// schemas-path shape).
func TestIntegrationListTablesLiveJSON(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	t.Setenv("CRDB_DSN", "")

	dbName := uniqueIntegDB(t, "lt_json")
	dropDB := setupIntegDB(t, cluster.DSN, dbName,
		"CREATE TABLE public.users (id INT8 PRIMARY KEY)",
	)
	t.Cleanup(dropDB)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"list-tables",
		"--dsn", dsnFor(t, cluster.DSN, dbName),
		"--output", "json",
	})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, output.TierConnected, env.Tier)
	require.Equal(t, output.ConnectionConnected, env.ConnectionStatus)
	require.Empty(t, env.Errors)

	var payload struct {
		Tables []conn.TableRef `json:"tables"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &payload))
	require.Equal(t, []conn.TableRef{{Schema: "public", Name: "users"}}, payload.Tables)
}

// TestIntegrationListTablesSchemaWinsOverDSN pins the CLI's
// precedence rule when both --schema and --dsn are supplied: the
// schema-file path wins silently, the DSN is ignored. This is
// asymmetric with the MCP layer (which errors on
// schemas-AND-dsn) but matches the CLI's "fallback" framing —
// --schema is the explicit input, --dsn is the ambient fallback. A
// future change that flipped this (e.g. errored, or had --dsn win)
// would silently break a workflow where users set CRDB_DSN globally
// and pass --schema for a one-off offline run.
func TestIntegrationListTablesSchemaWinsOverDSN(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	t.Setenv("CRDB_DSN", "")

	dbName := uniqueIntegDB(t, "lt_schema_wins")
	dropDB := setupIntegDB(t, cluster.DSN, dbName,
		"CREATE TABLE public.live_only (id INT8 PRIMARY KEY)",
	)
	t.Cleanup(dropDB)

	schemaFile := writeSchemaFile(t, "CREATE TABLE schema_only (id INT8 PRIMARY KEY);")

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"list-tables",
		"--schema", schemaFile,
		"--dsn", dsnFor(t, cluster.DSN, dbName),
	})

	require.NoError(t, root.Execute())
	require.Equal(t, "schema_only\n", stdout.String(),
		"--schema must win; --dsn must not contribute live_only")
}

// TestIntegrationListTablesLiveIncludeSystem pins the --include-system
// escape hatch: with the flag, system schemas (here pg_catalog) appear
// in the output; without it, they do not.
func TestIntegrationListTablesLiveIncludeSystem(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	t.Setenv("CRDB_DSN", "")

	dbName := uniqueIntegDB(t, "lt_sys")
	dropDB := setupIntegDB(t, cluster.DSN, dbName)
	t.Cleanup(dropDB)

	dsn := dsnFor(t, cluster.DSN, dbName)

	// Default: no system schemas.
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"list-tables", "--dsn", dsn})
	require.NoError(t, root.Execute())
	require.NotContains(t, stdout.String(), "pg_catalog.")

	// With --include-system: pg_catalog rows appear.
	root = newRootCmd()
	stdout.Reset()
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"list-tables", "--dsn", dsn, "--include-system"})
	require.NoError(t, root.Execute())
	require.Contains(t, stdout.String(), "pg_catalog.")
}

// TestIntegrationDescribeLiveJSON covers the happy path for the live
// describe fallback: SHOW CREATE round-trips through catalog.Load and
// the envelope reports tier=connected with the standard catalog.Table
// payload shape.
func TestIntegrationDescribeLiveJSON(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	t.Setenv("CRDB_DSN", "")

	dbName := uniqueIntegDB(t, "desc_json")
	dropDB := setupIntegDB(t, cluster.DSN, dbName,
		`CREATE TABLE public.users (
			id INT8 PRIMARY KEY,
			email STRING NOT NULL,
			INDEX users_email_idx (email)
		)`,
	)
	t.Cleanup(dropDB)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"describe", "users",
		"--dsn", dsnFor(t, cluster.DSN, dbName),
		"--output", "json",
	})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, output.TierConnected, env.Tier)
	require.Equal(t, output.ConnectionConnected, env.ConnectionStatus)
	require.Empty(t, env.Errors)

	var tbl catalog.Table
	require.NoError(t, json.Unmarshal(env.Data, &tbl))
	require.Equal(t, "users", tbl.Name)
	require.Equal(t, []string{"id"}, tbl.PrimaryKey)
	require.Len(t, tbl.Columns, 2)
	require.NotEmpty(t, tbl.Indexes)
}

// TestIntegrationDescribeLiveQualified pins the schema.table form on
// the live path: a qualified argument bypasses schema resolution and
// goes straight to SHOW CREATE.
func TestIntegrationDescribeLiveQualified(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	t.Setenv("CRDB_DSN", "")

	dbName := uniqueIntegDB(t, "desc_qual")
	dropDB := setupIntegDB(t, cluster.DSN, dbName,
		"CREATE SCHEMA app",
		"CREATE TABLE app.orders (id INT8 PRIMARY KEY)",
	)
	t.Cleanup(dropDB)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"describe", "app.orders",
		"--dsn", dsnFor(t, cluster.DSN, dbName),
	})

	require.NoError(t, root.Execute())
	require.Contains(t, stdout.String(), "Table: orders")
	require.Contains(t, stdout.String(), "Primary Key: id")
}

// TestIntegrationDescribeLiveAmbiguous covers the multi-schema
// ambiguity error: an unqualified name that resolves in two schemas
// must surface the candidate list and the suggestion to qualify.
// The error message stays compatible with the schemas-path "table %q
// not found"-style wording so users see one consistent vocabulary.
func TestIntegrationDescribeLiveAmbiguous(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	t.Setenv("CRDB_DSN", "")

	dbName := uniqueIntegDB(t, "desc_ambig")
	dropDB := setupIntegDB(t, cluster.DSN, dbName,
		"CREATE SCHEMA app",
		"CREATE TABLE public.users (id INT8 PRIMARY KEY)",
		"CREATE TABLE app.users (id INT8 PRIMARY KEY)",
	)
	t.Cleanup(dropDB)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"describe", "users",
		"--dsn", dsnFor(t, cluster.DSN, dbName),
		"--output", "json",
	})

	err := root.Execute()
	require.Error(t, err)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.NotEmpty(t, env.Errors)
	msg := env.Errors[0].Message
	require.Contains(t, msg, `"users"`)
	require.Contains(t, msg, "multiple schemas")
	require.Contains(t, msg, "app")
	require.Contains(t, msg, "public")
}

// TestIntegrationDescribeLiveNotFound covers the unqualified miss:
// the live path emits the same "table %q not found" error string as
// the schemas path so users do not have to learn two vocabularies.
func TestIntegrationDescribeLiveNotFound(t *testing.T) {
	cluster := cockroachtest.Shared(t)
	t.Setenv("CRDB_DSN", "")

	dbName := uniqueIntegDB(t, "desc_404")
	dropDB := setupIntegDB(t, cluster.DSN, dbName)
	t.Cleanup(dropDB)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"describe", "nope",
		"--dsn", dsnFor(t, cluster.DSN, dbName),
		"--output", "json",
	})

	err := root.Execute()
	require.Error(t, err)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.NotEmpty(t, env.Errors)
	require.Contains(t, env.Errors[0].Message, `table "nope" not found`)
}

// uniqueIntegDB returns a database identifier scoped to the calling
// test, so tests in this file can run in parallel against the shared
// cluster without colliding on database state.
func uniqueIntegDB(t *testing.T, prefix string) string {
	t.Helper()
	suffix := strings.ToLower(strings.NewReplacer("/", "_", "-", "_").Replace(t.Name()))
	return prefix + "_" + suffix
}

// setupIntegDB creates a fresh database, runs each setup statement in
// it, and returns a cleanup function that drops the database. The
// setup statements run with the new database as the current_database()
// so unqualified CREATE TABLE statements land in the right place
// without each call having to repeat the database name.
func setupIntegDB(t *testing.T, baseDSN, dbName string, setupSQL ...string) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := pgx.Connect(ctx, baseDSN)
	require.NoError(t, err, "open setup connection to base DSN")
	defer c.Close(ctx) //nolint:errcheck // best-effort

	_, err = c.Exec(ctx, "CREATE DATABASE "+dbName)
	require.NoError(t, err)

	if len(setupSQL) > 0 {
		dbConn, err := pgx.Connect(ctx, dsnFor(t, baseDSN, dbName))
		require.NoError(t, err, "open setup connection to per-test DB")
		defer dbConn.Close(ctx) //nolint:errcheck // best-effort

		for _, stmt := range setupSQL {
			_, err = dbConn.Exec(ctx, stmt)
			require.NoError(t, err, "exec setup SQL: %s", stmt)
		}
	}

	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, baseDSN)
		if err != nil {
			t.Logf("setupIntegDB cleanup: connect failed: %v", err)
			return
		}
		defer c.Close(ctx) //nolint:errcheck // best-effort
		if _, err := c.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName+" CASCADE"); err != nil {
			t.Logf("setupIntegDB cleanup: drop failed: %v", err)
		}
	}
}

// dsnFor returns a copy of dsn whose path component (the database
// name) is replaced with dbName. Manager intentionally never issues
// SET database, so per-test database selection has to ride the DSN.
func dsnFor(t *testing.T, dsn, dbName string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.Path = "/" + dbName
	return u.String()
}
