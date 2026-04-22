// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/pgwire/pgerror"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/spilchen/sql-ai-tools/internal/catalog"
	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/schemawarn"
)

// schemasParam is the wire name of the inline-schemas argument shared by
// list_tables, describe_table, and (optionally) validate_sql. Centralized
// so all three handlers' tool definitions stay in lockstep.
const schemasParam = "schemas"

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

// schemaLoadErrorCode is the fallback envelope code emitted when
// catalog.Load returns an error that carries no SQLSTATE — typically an
// I/O or validation failure rather than a parser error. Mirrors the
// CLI's renderSchemaLoadError fallback so both surfaces tag I/O-level
// schema failures identically.
const schemaLoadErrorCode = "schema_load_error"

// ListTablesTool returns the MCP tool definition for list_tables.
func ListTablesTool() mcp.Tool {
	return mcp.NewTool(
		ListTablesToolName,
		mcp.WithDescription(`List tables defined in one or more inline CREATE TABLE schemas. Returns an envelope whose data payload is {"tables": [...]} (always present, possibly empty). Loader warnings are surfaced as schema_warning entries in the envelope errors stream.`),
		mcp.WithArray(schemasParam,
			mcp.Required(),
			mcp.MinItems(1),
			mcp.Description("Inline schema sources; each {label, sql} maps to a catalog.SchemaSource. label is optional and only used in warnings/errors."),
			mcp.Items(schemasItemSchema()),
		),
	)
}

// DescribeTableTool returns the MCP tool definition for describe_table.
func DescribeTableTool() mcp.Tool {
	return mcp.NewTool(
		DescribeTableToolName,
		mcp.WithDescription(`Describe a single table from one or more inline CREATE TABLE schemas. Returns an envelope whose data payload is the catalog.Table JSON (name, columns, primary key, indexes). When the table is missing the envelope carries a 42P01 error with an "available_tables" context list.`),
		mcp.WithString("table",
			mcp.Required(),
			mcp.Description("Table name to describe (case-insensitive)."),
		),
		mcp.WithArray(schemasParam,
			mcp.Required(),
			mcp.MinItems(1),
			mcp.Description("Inline schema sources; each {label, sql} maps to a catalog.SchemaSource. label is optional and only used in warnings/errors."),
			mcp.Items(schemasItemSchema()),
		),
	)
}

// ListTablesHandler returns the handler for the list_tables tool. It
// builds an in-memory catalog from the inline schemas argument and
// emits the same {"tables": [...]} payload as the CLI list-tables
// command (including the nil-to-empty normalization so the slice
// always marshals as `[]`, never `null`).
func ListTablesHandler(parserVersion string) server.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sources, toolErr := extractSchemas(req, schemasRequired)
		if toolErr != nil {
			return toolErr, nil
		}

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
}

// DescribeTableHandler returns the handler for the describe_table tool.
// Unlike the CLI's `describe`, a missing table is surfaced as a
// structured 42P01 envelope error (with an "available_tables" context
// list) rather than a generic internal_error — the MCP envelope is
// designed for programmatic consumption and benefits from the real
// SQLSTATE.
func DescribeTableHandler(parserVersion string) server.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tableName, toolErr := extractRequiredString(req, "table")
		if toolErr != nil {
			return toolErr, nil
		}
		sources, toolErr := extractSchemas(req, schemasRequired)
		if toolErr != nil {
			return toolErr, nil
		}

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
}

// extractSchemas pulls the schemas argument from req and converts each
// entry into a catalog.SchemaSource. mode=schemasRequired rejects a
// missing or empty argument with a tool-level error (used by
// list_tables and describe_table); mode=schemasOptional treats those
// cases as "no schema provided" and returns (nil, nil) so the caller
// can take the schema-less path (used by validate_sql).
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
// table. available is the catalog's full TableNames() slice; when
// non-empty it is attached as the "available_tables" context so agents
// can suggest a correction. When empty the context key is omitted
// entirely so consumers do not have to special-case `null`.
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
