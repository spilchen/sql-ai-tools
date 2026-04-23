// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/pgwire/pgerror"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/spilchen/sql-ai-tools/internal/catalog"
	"github.com/spilchen/sql-ai-tools/internal/conn"
	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/schemawarn"
)

// schemasParam is the wire name of the inline-schemas argument shared by
// list_tables, describe_table, and (optionally) validate_sql. Centralized
// so all three handlers' tool definitions stay in lockstep.
const schemasParam = "schemas"

// dsnParam is the optional connection-string argument that opts
// list_tables / describe_table into live introspection. The handler
// rejects requests that supply both schemas and dsn so the source of
// truth for any one call is unambiguous.
const dsnParam = "dsn"

// includeSystemParam is the optional boolean that opts list_tables's
// live path into returning system schemas. Default false matches the
// CLI's --include-system default.
const includeSystemParam = "include_system"

// schemasRequirement controls whether extractSchemas accepts an
// absent/empty schemas argument. A typed alias for the requirement
// keeps call sites self-documenting (extractSchemas(req, schemasRequired))
// and prevents the boolean from being silently inverted by a future
// refactor.
type schemasRequirement bool

const (
	schemasOptional schemasRequirement = false
	schemasRequired schemasRequirement = true
)

// undefinedTableCode is the SQLSTATE for a missing table reference.
// describe_table returns this when the requested table is not present in
// the loaded catalog so agents can branch on it the same way they would
// on a real cluster's "relation does not exist" error.
const undefinedTableCode = "42P01"

// ambiguousTableCode is the envelope code emitted when an unqualified
// table name on the live path resolves in multiple non-system schemas.
// Reuses CockroachDB's pgwire SQLSTATE for ambiguous_alias so agents
// can branch on it the same way they would on a real cluster's
// disambiguation error.
const ambiguousTableCode = "42P09"

// schemaLoadErrorCode is the fallback envelope code emitted when
// catalog.Load returns an error that carries no SQLSTATE — typically an
// I/O or validation failure rather than a parser error. Mirrors the
// CLI's renderSchemaLoadError fallback so both surfaces tag I/O-level
// schema failures identically.
const schemaLoadErrorCode = "schema_load_error"

// ListTablesTool returns the MCP tool definition for list_tables. The
// tool now accepts either an inline `schemas` array (Tier 2,
// disconnected) or a `dsn` (Tier 3, live introspection); supplying
// both is a tool-level error so callers don't have to reason about
// precedence.
func ListTablesTool() mcp.Tool {
	return mcp.NewTool(
		ListTablesToolName,
		mcp.WithDescription(`List tables defined in one or more inline CREATE TABLE schemas, or — when a dsn is supplied instead — list tables in the connected cluster's current database via information_schema. Returns an envelope whose data payload is {"tables": [...]} (always present, possibly empty); the array elements are bare strings on the schemas path and {"schema","name"} objects on the live path. Loader warnings are surfaced as schema_warning entries in the envelope errors stream. Pass exactly one of schemas or dsn.`),
		mcp.WithArray(schemasParam,
			mcp.Description("Inline schema sources; each {label, sql} maps to a catalog.SchemaSource. label is optional and only used in warnings/errors. Mutually exclusive with dsn."),
			mcp.Items(schemasItemSchema()),
		),
		mcp.WithString(dsnParam,
			mcp.Description("CockroachDB connection string (postgres:// URI). Mutually exclusive with schemas; when supplied, list-tables falls back to live information_schema introspection in the DSN's database."),
		),
		mcp.WithBoolean(includeSystemParam,
			mcp.Description("On the live (dsn) path, broaden the listing to every relation visible in information_schema (system schemas, views, sequences). Ignored on the schemas path. Default false."),
		),
	)
}

// DescribeTableTool returns the MCP tool definition for describe_table.
// Like ListTablesTool, it accepts either an inline `schemas` array or
// a `dsn`; on the live path the table argument may be qualified
// ("schema.table") or left bare and resolved across non-system schemas.
func DescribeTableTool() mcp.Tool {
	return mcp.NewTool(
		DescribeTableToolName,
		mcp.WithDescription(`Describe a single table from one or more inline CREATE TABLE schemas, or — when a dsn is supplied instead — fetch and parse SHOW CREATE TABLE against the connected cluster. Returns an envelope whose data payload is the catalog.Table JSON (name, columns, primary key, indexes). When the table is missing the envelope carries a 42P01 error with an "available_tables" context list (schemas path) or no context (live path). When an unqualified live name is ambiguous, the envelope carries a 42P09 error with the candidate schemas. Pass exactly one of schemas or dsn.`),
		mcp.WithString("table",
			mcp.Required(),
			mcp.Description(`Table name to describe (case-insensitive). On the live path, may be qualified as "schema.table".`),
		),
		mcp.WithArray(schemasParam,
			mcp.Description("Inline schema sources; each {label, sql} maps to a catalog.SchemaSource. label is optional and only used in warnings/errors. Mutually exclusive with dsn."),
			mcp.Items(schemasItemSchema()),
		),
		mcp.WithString(dsnParam,
			mcp.Description("CockroachDB connection string (postgres:// URI). Mutually exclusive with schemas; when supplied, describe_table runs SHOW CREATE TABLE against the cluster."),
		),
	)
}

// ListTablesHandler returns the handler for the list_tables tool. It
// dispatches to the schema-file path or the live-cluster path based on
// which of (schemas, dsn) is supplied — exactly one is required.
func ListTablesHandler(parserVersion string) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sources, toolErr := extractSchemas(req, schemasOptional)
		if toolErr != nil {
			return toolErr, nil
		}
		dsn, toolErr := extractOptionalString(req, dsnParam)
		if toolErr != nil {
			return toolErr, nil
		}
		if toolErr := requireExactlyOneSource(sources, dsn); toolErr != nil {
			return toolErr, nil
		}

		if dsn != "" {
			includeSystem, toolErr := extractOptionalBool(req, includeSystemParam)
			if toolErr != nil {
				return toolErr, nil
			}
			return listTablesLive(ctx, parserVersion, dsn, includeSystem)
		}
		return listTablesFromSchemas(parserVersion, sources)
	}
}

// listTablesFromSchemas implements the historical Tier 2 schema-file
// branch of list_tables. It is split out so the live branch can sit
// alongside without bloating the handler closure.
func listTablesFromSchemas(parserVersion string, sources []catalog.SchemaSource) (*mcp.CallToolResult, error) {
	env := schemaFileEnvelope(parserVersion, "" /* targetVersion */)

	cat, err := catalog.Load(sources)
	if err != nil {
		env.Errors = []output.Error{schemaLoadEnvelopeError(err)}
		return envelopeResult(env)
	}
	schemawarn.Append(&env, cat)

	tables := cat.TableNames()
	if tables == nil {
		tables = []string{}
	}

	data, err := json.Marshal(struct {
		Tables []string `json:"tables"`
	}{Tables: tables})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
	}
	env.Data = data
	return envelopeResult(env)
}

// listTablesLive runs the Tier 3 live-cluster branch: open a Manager,
// query information_schema, and emit a payload of TableRef objects so
// agents can render qualified names. ConnectionStatus flips to
// connected only after a successful round-trip; pre-flight errors
// keep the disconnected status so the envelope reflects what
// happened.
func listTablesLive(ctx context.Context, parserVersion, dsn string, includeSystem bool) (*mcp.CallToolResult, error) {
	env := connectedEnvelope(parserVersion, "" /* targetVersion */)

	mgr := conn.NewManager(dsn)
	defer mgr.Close(ctx) //nolint:errcheck // best-effort cleanup

	tables, err := mgr.ListTablesFromCluster(ctx, conn.ListOptions{IncludeSystem: includeSystem})
	if err != nil {
		env.Errors = []output.Error{diag.FromClusterError(err, "")}
		return envelopeResult(env)
	}
	env.ConnectionStatus = output.ConnectionConnected

	data, err := json.Marshal(struct {
		Tables []conn.TableRef `json:"tables"`
	}{Tables: tables})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
	}
	env.Data = data
	return envelopeResult(env)
}

// DescribeTableHandler returns the handler for the describe_table tool.
// As in list_tables, exactly one of schemas / dsn must be supplied;
// the live path additionally accepts qualified table names and
// surfaces ambiguity as a 42P09 envelope error.
func DescribeTableHandler(parserVersion string) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tableName, toolErr := extractRequiredString(req, "table")
		if toolErr != nil {
			return toolErr, nil
		}
		sources, toolErr := extractSchemas(req, schemasOptional)
		if toolErr != nil {
			return toolErr, nil
		}
		dsn, toolErr := extractOptionalString(req, dsnParam)
		if toolErr != nil {
			return toolErr, nil
		}
		if toolErr := requireExactlyOneSource(sources, dsn); toolErr != nil {
			return toolErr, nil
		}

		if dsn != "" {
			return describeTableLive(ctx, parserVersion, dsn, tableName)
		}
		return describeTableFromSchemas(parserVersion, sources, tableName)
	}
}

func describeTableFromSchemas(parserVersion string, sources []catalog.SchemaSource, tableName string) (*mcp.CallToolResult, error) {
	env := schemaFileEnvelope(parserVersion, "" /* targetVersion */)

	cat, err := catalog.Load(sources)
	if err != nil {
		env.Errors = []output.Error{schemaLoadEnvelopeError(err)}
		return envelopeResult(env)
	}
	schemawarn.Append(&env, cat)

	tbl, ok := cat.Table(tableName)
	if !ok {
		env.Errors = append(env.Errors, tableNotFoundError(tableName, cat.TableNames()))
		return envelopeResult(env)
	}

	data, err := json.Marshal(tbl)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
	}
	env.Data = data
	return envelopeResult(env)
}

func describeTableLive(ctx context.Context, parserVersion, dsn, tableName string) (*mcp.CallToolResult, error) {
	env := connectedEnvelope(parserVersion, "" /* targetVersion */)

	mgr := conn.NewManager(dsn)
	defer mgr.Close(ctx) //nolint:errcheck // best-effort cleanup

	tbl, err := mgr.DescribeTableFromCluster(ctx, tableName)
	if err != nil {
		switch {
		case errors.Is(err, conn.ErrTableNotFound):
			// Live not-found has no available_tables context — that
			// list could be huge for a real cluster, so we only ship
			// it on the schemas path where we already have it in
			// memory. The 42P01 code is identical so consumers'
			// branching logic does not change.
			env.Errors = append(env.Errors, output.Error{
				Code:     undefinedTableCode,
				Severity: output.SeverityError,
				Message:  fmt.Sprintf("table %q not found", tableName),
				Category: diag.CategoryForCode(undefinedTableCode),
			})
		case errors.Is(err, conn.ErrAmbiguousTable):
			// errors.As is the data-carrier extraction; the guard is
			// defensive against a future caller that wraps
			// ErrAmbiguousTable plainly with %w (which would satisfy
			// errors.Is but leave ambig == nil and panic the next
			// line). Today the only producer is runDescribeTable
			// returning *AmbiguousTableError directly, so the
			// fallback branch is unreachable in practice — but the
			// CLI sibling has the same guard, and silently
			// nil-derefing this path would be a particularly nasty
			// regression to debug.
			var ambig *conn.AmbiguousTableError
			if errors.As(err, &ambig) {
				env.Errors = append(env.Errors, ambiguousTableEnvelopeError(ambig))
			} else {
				env.Errors = []output.Error{diag.FromClusterError(err, "")}
			}
		default:
			env.Errors = []output.Error{diag.FromClusterError(err, "")}
		}
		return envelopeResult(env)
	}
	env.ConnectionStatus = output.ConnectionConnected

	data, err := json.Marshal(tbl)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
	}
	env.Data = data
	return envelopeResult(env)
}

// extractSchemas pulls the schemas argument from req and converts each
// entry into a catalog.SchemaSource. mode=schemasRequired rejects a
// missing or empty argument with a tool-level error; mode=schemasOptional
// treats those cases as "no schema provided" and returns (nil, nil) so
// the caller can take the schema-less path.
//
// On any shape error (wrong type at the array level, non-object item,
// missing/empty sql, wrong type for label/sql) it returns a pre-built
// tool error result that the handler should return verbatim.
func extractSchemas(req mcp.CallToolRequest, mode schemasRequirement) ([]catalog.SchemaSource, *mcp.CallToolResult) {
	args := req.GetArguments()
	raw, exists := args[schemasParam]
	if !exists || raw == nil {
		if mode == schemasRequired {
			return nil, mcp.NewToolResultError(fmt.Sprintf("%s parameter is required", schemasParam))
		}
		return nil, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, mcp.NewToolResultError(fmt.Sprintf("%s parameter must be an array, got %T", schemasParam, raw))
	}
	if len(arr) == 0 {
		if mode == schemasRequired {
			return nil, mcp.NewToolResultError(fmt.Sprintf("%s parameter must contain at least one entry", schemasParam))
		}
		return nil, nil
	}

	sources := make([]catalog.SchemaSource, 0, len(arr))
	for i, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, mcp.NewToolResultError(fmt.Sprintf("%s[%d] must be an object, got %T", schemasParam, i, item))
		}

		rawSQL, exists := obj["sql"]
		if !exists {
			return nil, mcp.NewToolResultError(fmt.Sprintf("%s[%d].sql is required", schemasParam, i))
		}
		sqlStr, ok := rawSQL.(string)
		if !ok {
			return nil, mcp.NewToolResultError(fmt.Sprintf("%s[%d].sql must be a string, got %T", schemasParam, i, rawSQL))
		}
		sqlStr = strings.TrimSpace(sqlStr)
		if sqlStr == "" {
			return nil, mcp.NewToolResultError(fmt.Sprintf("%s[%d].sql must not be empty", schemasParam, i))
		}

		var label string
		if rawLabel, exists := obj["label"]; exists && rawLabel != nil {
			label, ok = rawLabel.(string)
			if !ok {
				return nil, mcp.NewToolResultError(fmt.Sprintf("%s[%d].label must be a string, got %T", schemasParam, i, rawLabel))
			}
		}

		sources = append(sources, catalog.SchemaSource{SQL: sqlStr, Label: label})
	}
	return sources, nil
}

// extractOptionalString returns a trimmed string parameter or "" if
// the argument is absent / nil. A non-string value is a hard tool
// error — there is no reasonable interpretation, and a malformed
// argument is more useful surfaced than silently dropped.
func extractOptionalString(req mcp.CallToolRequest, name string) (string, *mcp.CallToolResult) {
	raw, exists := req.GetArguments()[name]
	if !exists || raw == nil {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", mcp.NewToolResultError(fmt.Sprintf("%s parameter must be a string, got %T", name, raw))
	}
	return strings.TrimSpace(s), nil
}

// extractOptionalBool returns a boolean parameter or false if the
// argument is absent / nil. Non-bool values are a hard tool error,
// matching extractOptionalString's strictness.
func extractOptionalBool(req mcp.CallToolRequest, name string) (bool, *mcp.CallToolResult) {
	raw, exists := req.GetArguments()[name]
	if !exists || raw == nil {
		return false, nil
	}
	b, ok := raw.(bool)
	if !ok {
		return false, mcp.NewToolResultError(fmt.Sprintf("%s parameter must be a boolean, got %T", name, raw))
	}
	return b, nil
}

// requireExactlyOneSource enforces the schemas-XOR-dsn contract for
// list_tables and describe_table. Both empty rejects the call with a
// "must supply" error; both populated rejects with a "mutually
// exclusive" error so the caller does not have to reason about
// precedence between an inline schema dump and a live cluster.
func requireExactlyOneSource(sources []catalog.SchemaSource, dsn string) *mcp.CallToolResult {
	hasSchemas := len(sources) > 0
	hasDSN := dsn != ""
	switch {
	case !hasSchemas && !hasDSN:
		return mcp.NewToolResultError(fmt.Sprintf("must supply either %s or %s", schemasParam, dsnParam))
	case hasSchemas && hasDSN:
		return mcp.NewToolResultError(fmt.Sprintf("%s and %s are mutually exclusive; supply exactly one", schemasParam, dsnParam))
	}
	return nil
}

// schemaLoadEnvelopeError converts a catalog.Load error into the
// envelope's error shape. It mirrors cmd/root.go::renderSchemaLoadError:
// pgerror.GetPGCode is consulted first so a parser-side SQLSTATE (e.g.
// 42601 from a malformed CREATE TABLE) is propagated; failures with no
// SQLSTATE — typically I/O — fall back to schemaLoadErrorCode.
func schemaLoadEnvelopeError(err error) output.Error {
	code := pgerror.GetPGCode(err).String()
	if code == "" || code == "XXUUU" {
		code = schemaLoadErrorCode
	}
	return output.Error{
		Code:     code,
		Severity: output.SeverityError,
		Message:  err.Error(),
		Category: diag.CategoryForCode(code),
	}
}

// tableNotFoundError builds the 42P01 envelope entry for a missing
// table on the schemas path. available is the catalog's full
// TableNames() slice; when non-empty it is attached as the
// "available_tables" context so agents can suggest a correction.
// When empty the context key is omitted entirely so consumers do not
// have to special-case `null`.
func tableNotFoundError(name string, available []string) output.Error {
	err := output.Error{
		Code:     undefinedTableCode,
		Severity: output.SeverityError,
		Message:  fmt.Sprintf("table %q not found", name),
		Category: diag.CategoryForCode(undefinedTableCode),
	}
	if len(available) > 0 {
		err.Context = map[string]any{"available_tables": available}
	}
	return err
}

// ambiguousTableEnvelopeError builds the 42P09 envelope entry for a
// live-path table name that resolved in multiple schemas. The
// candidate schemas ride along as context so an agent can re-issue
// the call with a qualified name.
func ambiguousTableEnvelopeError(ambig *conn.AmbiguousTableError) output.Error {
	return output.Error{
		Code:     ambiguousTableCode,
		Severity: output.SeverityError,
		Message:  ambig.Error(),
		Category: diag.CategoryForCode(ambiguousTableCode),
		Context:  map[string]any{"schemas": ambig.Schemas},
	}
}

// schemasItemSchema returns the JSON Schema fragment describing a
// single entry of the schemas array. Shared between the three tool
// definitions so the wire schema cannot drift.
func schemasItemSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"label": map[string]any{
				"type":        "string",
				"description": "Optional human-readable name used in warnings/errors.",
			},
			"sql": map[string]any{
				"type":        "string",
				"description": "CREATE TABLE DDL statements for this source.",
			},
		},
		"required":             []string{"sql"},
		"additionalProperties": false,
	}
}
