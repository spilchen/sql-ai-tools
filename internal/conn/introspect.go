// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package conn

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/spilchen/sql-ai-tools/internal/catalog"
)

// pgUndefinedTable is the SQLSTATE returned by CockroachDB when a SHOW
// CREATE TABLE references a table the cluster cannot resolve. Mapping
// this to ErrTableNotFound lets a qualified-but-missing argument
// (e.g. "public.does_not_exist") surface the same way as an
// unqualified miss caught at the resolution step.
const pgUndefinedTable = "42P01"

// isUndefinedTableErr reports whether err's chain carries a pgwire
// "undefined table" (42P01) PgError. Uses errors.As so a wrapped
// %w-chain (the form Manager methods always produce) is inspected
// transparently.
func isUndefinedTableErr(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUndefinedTable
}

// systemSchemas is the set of schemas excluded from ListTablesFromCluster
// and the schema-resolution scan in DescribeTableFromCluster when the
// caller has not opted into system entries. These are CockroachDB's
// built-in catalogs (Postgres-compatible and CRDB-specific) plus the
// `system` database's own user-table-shaped tables; agents enumerating
// "the user's tables" almost never want them.
var systemSchemas = []string{
	"pg_catalog",
	"crdb_internal",
	"information_schema",
	"system",
}

// ErrTableNotFound is returned by DescribeTableFromCluster when no
// non-system schema in the connection's current database holds a table
// with the requested name. CLI and MCP layers map it to the existing
// "table %q not found" diagnostic so the live and schema-file paths
// surface the same shape.
var ErrTableNotFound = errors.New("table not found")

// ErrAmbiguousTable is returned by DescribeTableFromCluster when an
// unqualified table name resolves in more than one non-system schema.
// Callers should render the candidate list (carried in
// AmbiguousTableError) and prompt the user to qualify with
// schema.table.
var ErrAmbiguousTable = errors.New("table name is ambiguous across schemas")

// AmbiguousTableError wraps ErrAmbiguousTable with the candidate schema
// list so callers can render a helpful "exists in: a, b" hint without
// re-querying. errors.Is(err, ErrAmbiguousTable) holds.
type AmbiguousTableError struct {
	TableName string
	Schemas   []string
}

func (e *AmbiguousTableError) Error() string {
	return fmt.Sprintf("table %q exists in multiple schemas: %s",
		e.TableName, strings.Join(e.Schemas, ", "))
}

func (e *AmbiguousTableError) Unwrap() error { return ErrAmbiguousTable }

// TableRef identifies a table by its schema and name. Returned by
// ListTablesFromCluster so callers can render qualified names without
// re-querying when the listing spans multiple schemas in the same
// database.
type TableRef struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
}

// ListOptions controls which schemas ListTablesFromCluster returns.
// The zero value excludes the system schemas listed in systemSchemas
// (pg_catalog, crdb_internal, information_schema, system), which is the
// right default for an agent enumerating a user database. Setting
// IncludeSystem=true returns every schema, intended as an escape hatch
// for users debugging catalog visibility.
type ListOptions struct {
	IncludeSystem bool
}

// ListTablesFromCluster returns user tables in the connection's current
// database, ordered by (schema, name). The slice is always non-nil so
// the JSON encoder emits `[]` rather than `null`. Whether system
// schemas are included is controlled by opts.IncludeSystem.
//
// On any query/scan failure after a successful connect, the underlying
// connection is closed and the Manager reverts to its pre-connect
// state, mirroring the recovery contract documented on Ping/Explain.
func (m *Manager) ListTablesFromCluster(ctx context.Context, opts ListOptions) ([]TableRef, error) {
	if err := m.connect(ctx); err != nil {
		return nil, err
	}

	tables, err := m.runListTables(ctx, opts)
	if err != nil {
		m.conn.Close(ctx) //nolint:errcheck // best-effort cleanup
		m.conn = nil
		return nil, err
	}
	return tables, nil
}

// runListTables is the inner half of ListTablesFromCluster that owns
// the query/scan pipeline. Splitting it out lets the public method
// centralize the close-and-nil recovery so all failure modes (Query,
// Scan, rows.Err) collapse onto one cleanup path.
func (m *Manager) runListTables(ctx context.Context, opts ListOptions) ([]TableRef, error) {
	// information_schema.tables exposes table_catalog (database) and
	// table_schema (namespace within the database). Filter on
	// current_database() so a Manager bound to "movr" never reports
	// tables in some other database the user happens to have access
	// to.
	//
	// Default: BASE TABLE only and skip the system schemas — what
	// "list tables" reads as in plain English. IncludeSystem opens
	// both gates: the system-schema filter drops away (so pg_catalog
	// and crdb_internal show up), and so does the table_type filter
	// (because pg_catalog tables are technically VIEWs, and limiting
	// to BASE TABLE would hide them even with the schema filter
	// off). The escape hatch is intentionally broad: a user reaching
	// for it usually wants "show me everything".
	const selectFrom = `
SELECT table_schema, table_name
FROM   information_schema.tables
WHERE  table_catalog = current_database()`

	var (
		query string
		args  []any
	)
	if opts.IncludeSystem {
		query = selectFrom + " ORDER BY table_schema, table_name"
	} else {
		query = selectFrom +
			" AND table_type = 'BASE TABLE'" +
			" AND table_schema <> ALL($1)" +
			" ORDER BY table_schema, table_name"
		args = []any{systemSchemas}
	}

	rows, err := m.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()

	tables := []TableRef{}
	for rows.Next() {
		var ref TableRef
		if err := rows.Scan(&ref.Schema, &ref.Name); err != nil {
			return nil, fmt.Errorf("scan list-tables row: %w", err)
		}
		tables = append(tables, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read list-tables rows: %w", err)
	}
	return tables, nil
}

// DescribeTableFromCluster fetches and parses the CREATE statement for
// a single table. tableName may be unqualified ("users") or qualified
// ("public.users"); a three-part `db.schema.table` is rejected because
// the Manager intentionally only sees its DSN's database.
//
// Resolution always goes through information_schema first so the
// cluster-stored case for the schema and table names is used in the
// subsequent SHOW CREATE TABLE — this is what makes lookups
// case-insensitive even when CRDB stored the identifier in mixed
// case (e.g. via CREATE TABLE "Users"). Zero matches returns
// ErrTableNotFound; for unqualified names, multiple matches return
// *AmbiguousTableError (which unwraps to ErrAmbiguousTable) so the
// caller can render the candidate schema list.
//
// SHOW CREATE TABLE then returns the reconstructed DDL; that DDL is
// fed back through catalog.Load so the returned catalog.Table has the
// same shape as the schema-file path produces, and the existing
// CLI/MCP renderers consume both paths uniformly.
//
// Recovery contract: on query/scan/parse failures *other than*
// ErrTableNotFound and *AmbiguousTableError, the underlying
// connection is closed and the Manager reverts to its pre-connect
// state (mirroring Explain's recovery contract). The two
// resolution-result errors are deliberately exempt so a "did you
// mean?" retry — which is a normal user flow on the CLI — does not
// have to pay for a re-dial.
func (m *Manager) DescribeTableFromCluster(ctx context.Context, tableName string) (catalog.Table, error) {
	if err := m.connect(ctx); err != nil {
		return catalog.Table{}, err
	}

	tbl, err := m.runDescribeTable(ctx, tableName)
	if err != nil {
		if errors.Is(err, ErrTableNotFound) || errors.Is(err, ErrAmbiguousTable) {
			return catalog.Table{}, err
		}
		m.conn.Close(ctx) //nolint:errcheck // best-effort cleanup
		m.conn = nil
		return catalog.Table{}, err
	}
	return tbl, nil
}

func (m *Manager) runDescribeTable(ctx context.Context, tableName string) (catalog.Table, error) {
	schemaIn, tableIn, err := splitTableName(tableName)
	if err != nil {
		return catalog.Table{}, err
	}

	schema, table, err := m.resolveTable(ctx, schemaIn, tableIn)
	if err != nil {
		return catalog.Table{}, err
	}

	createStmt, err := m.fetchCreateStatement(ctx, schema, table)
	if err != nil {
		return catalog.Table{}, err
	}

	cat, err := catalog.Load([]catalog.SchemaSource{{
		SQL:   createStmt,
		Label: schema + "." + table,
	}})
	if err != nil {
		return catalog.Table{}, fmt.Errorf("parse SHOW CREATE output: %w", err)
	}

	tbl, ok := cat.Table(table)
	if !ok {
		// SHOW CREATE TABLE returned a statement that did not parse
		// into a table named `table`; this means CRDB renamed,
		// transformed, or omitted the identifier in a way the loader
		// could not reconcile. Surface a strict error so a future
		// SHOW-CREATE format change fails loudly here rather than
		// silently returning the zero value.
		return catalog.Table{}, fmt.Errorf(
			"SHOW CREATE TABLE %q.%q produced no recognized table definition",
			schema, table)
	}
	return tbl, nil
}

// splitTableName parses a user-supplied table argument into (schema,
// table). Valid forms:
//
//	"users"        -> ("", "users")           // resolve schema later
//	"public.users" -> ("public", "users")
//
// A three-part `db.schema.table` is rejected: cross-database lookups
// require a different DSN, not a different argument. Empty input and
// empty halves of a qualified name are also rejected so downstream
// SQL never receives a blank identifier.
func splitTableName(name string) (schema, table string, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", fmt.Errorf("table name must not be empty")
	}

	parts := strings.Split(name, ".")
	switch len(parts) {
	case 1:
		return "", parts[0], nil
	case 2:
		if parts[0] == "" || parts[1] == "" {
			return "", "", fmt.Errorf("invalid qualified table name %q: schema and table must both be non-empty", name)
		}
		return parts[0], parts[1], nil
	default:
		return "", "", fmt.Errorf(
			"invalid table name %q: only \"table\" or \"schema.table\" are accepted (database is fixed by the DSN)",
			name)
	}
}

// resolveTable looks up the cluster-stored case for (schema, table)
// in information_schema. schemaIn=="" makes the lookup span every
// non-system schema in the current database; a non-empty schemaIn
// constrains it to that one schema. Both inputs are matched
// case-insensitively (lower($1) = lower(table_*)) so a user-supplied
// "Users" still resolves a stored "users", and so does the inverse.
//
// Returning the cluster-stored case (rather than echoing the user's
// input) is what lets the caller's SHOW CREATE TABLE call land on the
// right relation: SHOW CREATE expects the actual identifier, and
// pgx.Identifier.Sanitize quotes it — so passing "Users" against a
// stored "users" would produce a 42P01 even though the table exists.
//
// Outcomes:
//   - exactly one match → returns (storedSchema, storedTable, nil)
//   - zero matches → ErrTableNotFound (wrapped with the user-supplied
//     name for a useful message)
//   - multiple matches (only possible when schemaIn=="") →
//     *AmbiguousTableError carrying the candidate schemas
func (m *Manager) resolveTable(ctx context.Context, schemaIn, tableIn string) (string, string, error) {
	const baseQuery = `
SELECT table_schema, table_name
FROM   information_schema.tables
WHERE  table_catalog       = current_database()
  AND  table_type          = 'BASE TABLE'
  AND  lower(table_name)   = lower($1)`

	var (
		query string
		args  []any
	)
	if schemaIn == "" {
		query = baseQuery + ` AND table_schema <> ALL($2) ORDER BY table_schema`
		args = []any{tableIn, systemSchemas}
	} else {
		query = baseQuery + ` AND lower(table_schema) = lower($2)`
		args = []any{tableIn, schemaIn}
	}

	rows, err := m.conn.Query(ctx, query, args...)
	if err != nil {
		return "", "", fmt.Errorf("resolve table: %w", err)
	}
	defer rows.Close()

	type match struct{ schema, table string }
	var matches []match
	for rows.Next() {
		var pair match
		if err := rows.Scan(&pair.schema, &pair.table); err != nil {
			return "", "", fmt.Errorf("scan table-resolution row: %w", err)
		}
		matches = append(matches, pair)
	}
	if err := rows.Err(); err != nil {
		return "", "", fmt.Errorf("read table-resolution rows: %w", err)
	}

	switch len(matches) {
	case 0:
		if schemaIn != "" {
			return "", "", fmt.Errorf("%w: %s.%s", ErrTableNotFound, schemaIn, tableIn)
		}
		return "", "", fmt.Errorf("%w: %q", ErrTableNotFound, tableIn)
	case 1:
		return matches[0].schema, matches[0].table, nil
	default:
		schemas := make([]string, len(matches))
		for i, m := range matches {
			schemas[i] = m.schema
		}
		return "", "", &AmbiguousTableError{TableName: tableIn, Schemas: schemas}
	}
}

// fetchCreateStatement runs SHOW CREATE TABLE on the qualified
// identifier and returns the second column (the reconstructed DDL).
// The schema and table are escaped via pgx.Identifier.Sanitize so
// user-supplied names cannot break out of the statement; SHOW does not
// accept placeholders for identifiers, so safe escaping is the only
// option. A pgwire error whose SQLSTATE is 42P01 is rewritten as
// ErrTableNotFound so a qualified-but-missing argument surfaces
// identically to an unqualified miss.
func (m *Manager) fetchCreateStatement(ctx context.Context, schema, table string) (string, error) {
	qualified := pgx.Identifier{schema, table}.Sanitize()
	query := "SHOW CREATE TABLE " + qualified

	var (
		ignoredName string
		createStmt  string
	)
	err := m.conn.QueryRow(ctx, query).Scan(&ignoredName, &createStmt)
	if err != nil {
		if isUndefinedTableErr(err) {
			return "", fmt.Errorf("%w: %s.%s", ErrTableNotFound, schema, table)
		}
		return "", fmt.Errorf("SHOW CREATE TABLE %s: %w", qualified, err)
	}
	return createStmt, nil
}
