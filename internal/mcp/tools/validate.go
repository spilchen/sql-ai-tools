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
		mcp.WithDescription("Validate SQL for syntax, type, and (with the schemas argument) name errors. Returns an envelope whose data is {\"valid\": true, \"checks\": {syntax, type_check, name_resolution}}; each check is \"ok\" or \"skipped\". Failures are reported as structured envelope errors with SQLSTATE codes, severity, message, and source position. Tolerates cockroach sql REPL paste artifacts (leading `root@host:port/db>` prompt and `-> ` continuation prompts). Pass raw paste in one shot; do not pre-strip."),
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

		originalSQL := sql
		strip := preprocessSQL(&env, sql)
		sql = strip.Stripped

		before := len(env.Errors)
		stmts, parseErr := parser.Parse(sql)
		if parseErr != nil {
			env.Errors = append(env.Errors, diag.FromParseError(parseErr, sql))
			translateErrorPositions(&env, before, originalSQL, strip)
			return failureEnvelope(env, validateresult.Checks{
				Syntax:             validateresult.CheckFailed,
				TypeCheck:          validateresult.CheckSkipped,
				FunctionResolution: validateresult.CheckSkipped,
				NameResolution:     validateresult.CheckSkipped,
			})
		}

		// Version-aware feature warnings are advisory and emitted
		// regardless of whether semantic checks pass — see the CLI
		// path's matching comment for rationale.
		env.Errors = append(env.Errors, version.Inspect(stmts, target, nil)...)

		checks := validateresult.Checks{Syntax: validateresult.CheckOK}

		var cat *catalog.Catalog
		if len(sources) == 0 {
			env.Errors = append(env.Errors, validateresult.CapabilityRequiredError(
				validateresult.CapabilityNameResolution,
				"name resolution skipped: schemas not provided",
				"pass the schemas argument to enable table name resolution",
			))
		} else {
			loaded, err := catalog.Load(sources)
			if err != nil {
				env.Errors = append(env.Errors, schemaLoadEnvelopeError(err))
				return envelopeResult(env)
			}
			cat = loaded
			schemawarn.Append(&env, cat)
		}

		semBefore := len(env.Errors)
		semRes, semErrs := semcheck.Run(stmts, sql, cat)
		checks.TypeCheck = semRes.TypeCheck
		checks.FunctionResolution = semRes.FunctionResolution
		checks.NameResolution = semRes.NameResolution

		if len(semErrs) > 0 {
			env.Errors = append(env.Errors, semErrs...)
			translateErrorPositions(&env, semBefore, originalSQL, strip)
			return failureEnvelope(env, checks)
		}

		data, err := json.Marshal(validateresult.Result{Valid: true, Checks: checks})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
		}
		env.Data = data
		return envelopeResult(env)
	}
}

// failureEnvelope marshals checks into env.Data as a Result{Valid:false}
// payload before delegating to envelopeResult. Sharing this helper across
// the parse-error and semantic-error branches keeps the failure-path
// JSON shape identical so consumers can branch on Result.Checks without
// special-casing the producing phase. Callers append every diagnostic
// to env.Errors before invoking failureEnvelope, so that even an
// (essentially impossible) marshal failure of the tiny Result struct
// still emits the diagnostics through the standard envelope path
// rather than collapsing them into an opaque tool-level error.
func failureEnvelope(env output.Envelope, checks validateresult.Checks) (*mcp.CallToolResult, error) {
	data, err := json.Marshal(validateresult.Result{Valid: false, Checks: checks})
	if err != nil {
		env.Errors = append(env.Errors, output.Error{
			Code:     "internal_error",
			Severity: output.SeverityError,
			Message:  fmt.Sprintf("encode result: %v", err),
		})
		return envelopeResult(env)
	}
	env.Data = data
	return envelopeResult(env)
}
