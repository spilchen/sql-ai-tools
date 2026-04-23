// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/spilchen/sql-ai-tools/internal/catalog"
	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/schemawarn"
	"github.com/spilchen/sql-ai-tools/internal/semcheck"
	"github.com/spilchen/sql-ai-tools/internal/validateresult"
	"github.com/spilchen/sql-ai-tools/internal/version"
)

// ValidateSQLTool returns the MCP tool definition for validate_sql.
// The optional schemas parameter mirrors the CLI's --schema flag: when
// supplied, table references are resolved against the inline catalog
// (Tier 2); when omitted, name resolution is skipped and a
// capability_required warning is appended to the envelope so agents
// can detect the missing capability rather than silently trust a
// partial result. The optional target_version parameter follows the
// shared per-call convention documented on TargetVersionParamName.
func ValidateSQLTool() mcp.Tool {
	return mcp.NewTool(
		ValidateSQLToolName,
		mcp.WithDescription(`Validate SQL for syntax, type, and (with the schemas argument) name errors. Returns an envelope whose data is {"valid": true, "checks": {syntax, type_check, name_resolution}}; each check is "ok" or "skipped". Failures are reported as structured envelope errors with SQLSTATE codes, severity, message, and source position.`),
		mcp.WithString("sql", mcp.Required(), mcp.Description("SQL string to validate")),
		mcp.WithString(TargetVersionParamName, mcp.Description(TargetVersionParamDescription)),
		mcp.WithArray(schemasParam,
			mcp.Description("Optional inline schema sources for table name resolution; each {label, sql} maps to a catalog.SchemaSource. label is optional. Omit (or pass an empty array) to run syntax/type checks only — name resolution will be reported as skipped via a capability_required warning."),
			mcp.Items(schemasItemSchema()),
		),
	)
}

// ValidateSQLHandler returns the handler for the validate_sql tool.
// On the success path the envelope's tier reflects whether schemas
// were supplied — schema_file when present, zero_config otherwise —
// and the data payload always carries a validateresult.Result so
// agents can branch on which phases ran. defaultTargetVersion is the
// server-level default; per-call target_version arguments override it.
// The resolved target is stamped onto the envelope on both tiers so
// that supplying schemas does not silently discard a target_version
// the client took the trouble to validate.
func ValidateSQLHandler(parserVersion, defaultTargetVersion string) server.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sql, toolErr := extractSQL(req)
		if toolErr != nil {
			return toolErr, nil
		}
		target, toolErr := resolveTargetVersion(req, defaultTargetVersion)
		if toolErr != nil {
			return toolErr, nil
		}
		sources, toolErr := extractSchemas(req, schemasOptional)
		if toolErr != nil {
			return toolErr, nil
		}

		var env output.Envelope
		if len(sources) == 0 {
			env = baseEnvelope(parserVersion, target)
		} else {
			env = schemaFileEnvelope(parserVersion, target)
		}

		stmts, parseErr := parser.Parse(sql)
		if parseErr != nil {
			env.Errors = append(env.Errors, diag.FromParseError(parseErr, sql))
			return envelopeResult(env)
		}

		if typeErrs := semcheck.CheckExprTypes(stmts, sql); len(typeErrs) > 0 {
			env.Errors = append(env.Errors, typeErrs...)
			return envelopeResult(env)
		}

		env.Errors = append(env.Errors, version.Inspect(stmts, target, nil)...)

		checks := validateresult.Checks{
			Syntax:    validateresult.CheckOK,
			TypeCheck: validateresult.CheckOK,
		}

		if len(sources) == 0 {
			checks.NameResolution = validateresult.CheckSkipped
			env.Errors = append(env.Errors, validateresult.CapabilityRequiredError(
				validateresult.CapabilityNameResolution,
				"name resolution skipped: schemas not provided",
				"pass the schemas argument to enable table name resolution",
			))
		} else {
			cat, err := catalog.Load(sources)
			if err != nil {
				env.Errors = append(env.Errors, schemaLoadEnvelopeError(err))
				return envelopeResult(env)
			}
			schemawarn.Append(&env, cat)
			nameErrs := semcheck.CheckTableNames(stmts, sql, cat)
			nameErrs = append(nameErrs, semcheck.CheckColumnNames(stmts, sql, cat)...)
			if len(nameErrs) > 0 {
				env.Errors = append(env.Errors, nameErrs...)
				return envelopeResult(env)
			}
			checks.NameResolution = validateresult.CheckOK
		}

		data, err := json.Marshal(validateresult.Result{Valid: true, Checks: checks})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
		}
		env.Data = data
		return envelopeResult(env)
	}
}
