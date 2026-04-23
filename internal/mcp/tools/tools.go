// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package tools provides MCP tool handler constructors for the SQL
// tools. The Tier 1 (zero-config) tools are parse_sql, validate_sql,
// format_sql, detect_risky_query, and summarize_sql; the Tier 2
// (schema_file) tools are list_tables and describe_table; the Tier 3
// (connected) tools are explain_sql and explain_schema_change, which
// require a per-call DSN since the MCP server holds no per-session
// connection state.
//
// Each handler returns the same output.Envelope JSON shape that the CLI
// emits under --output=json, so MCP clients get structured errors,
// parser version, and tier metadata consistent with the CLI surface.
//
// Tool-level errors (mcp.NewToolResultError) are reserved for
// infrastructure problems — missing or invalid parameters. SQL errors
// (syntax, type mismatch) are returned as successful tool results with
// the envelope's Errors array populated, because the tool itself
// succeeded; the SQL is simply invalid.
package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/spilchen/sql-ai-tools/internal/conn"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/safety"
)

// Registered MCP tool names.
const (
	ParseSQLToolName            = "parse_sql"
	ValidateSQLToolName         = "validate_sql"
	FormatSQLToolName           = "format_sql"
	DetectRiskyQueryToolName    = "detect_risky_query"
	SummarizeSQLToolName        = "summarize_sql"
	ExplainSQLToolName          = "explain_sql"
	ExplainSchemaChangeToolName = "explain_schema_change"
	ListTablesToolName          = "list_tables"
	DescribeTableToolName       = "describe_table"
)

// TargetVersionParamName is the optional MCP tool parameter name that
// lets a client override the server's default target CockroachDB
// version on a per-call basis. Tools that accept it follow the same
// validation rules as the CLI's --target-version flag (see
// output.ValidateTargetVersion).
const TargetVersionParamName = "target_version"

// TargetVersionParamDescription is the shared MCP-schema description
// for the target_version parameter so every tool documents the
// argument identically.
const TargetVersionParamDescription = "Optional target CockroachDB version (MAJOR.MINOR or MAJOR.MINOR.PATCH, with optional leading 'v'). Overrides the server-level default for this call."

// ModeParamName is the optional MCP tool parameter name that lets a
// client override the safety mode applied before any cluster contact.
// Mirrors the CLI's --mode flag. Tier 3 tools that accept it run the
// safety.Check allowlist before opening a connection.
const ModeParamName = "mode"

// ModeParamDescription is the shared MCP-schema description for the
// mode parameter so every Tier 3 tool documents the argument
// identically.
const ModeParamDescription = `Optional safety mode applied before any cluster contact. Defaults to "read_only", which permits non-mutating statements only. "safe_write" and "full_access" are reserved for follow-up work and currently reject every statement.`

// StatementTimeoutParamName is the optional MCP tool parameter name
// that lets a client override the per-call SET LOCAL statement_timeout
// applied inside the read-only transaction wrapper. The value is in
// milliseconds (numeric) so the wire format matches MCP's JSON
// type-system without parsing duration strings.
const StatementTimeoutParamName = "statement_timeout_ms"

// StatementTimeoutParamDescription is the shared MCP-schema
// description for the statement_timeout_ms parameter so every Tier 3
// tool documents the argument identically.
const StatementTimeoutParamDescription = "Optional statement_timeout (milliseconds) applied inside the read-only transaction wrapper. Default 30000 (30s). Non-positive values fall back to the default."

// extractRequiredString validates and returns a required string
// parameter from an MCP tool request. Leading and trailing whitespace
// is trimmed so all handlers behave consistently. On success, the
// returned *mcp.CallToolResult is nil. On failure (missing, wrong type,
// empty after trimming), it is a pre-built tool error that the caller
// should return immediately.
func extractRequiredString(req mcp.CallToolRequest, name string) (string, *mcp.CallToolResult) {
	raw, exists := req.GetArguments()[name]
	if !exists {
		return "", mcp.NewToolResultError(fmt.Sprintf("%s parameter is required", name))
	}
	s, ok := raw.(string)
	if !ok {
		return "", mcp.NewToolResultError(fmt.Sprintf("%s parameter must be a string, got %T", name, raw))
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", mcp.NewToolResultError(fmt.Sprintf("%s parameter must not be empty", name))
	}
	return s, nil
}

// extractSQL is a thin convenience wrapper around extractRequiredString
// for the common "sql" parameter, kept so the SQL-input handlers stay
// terse.
func extractSQL(req mcp.CallToolRequest) (string, *mcp.CallToolResult) {
	return extractRequiredString(req, "sql")
}

// baseEnvelope returns a pre-populated Envelope for Tier 1 (zero-config,
// disconnected) tools. When targetVersion is non-empty it is stamped
// onto the envelope and, if it differs from parserVersion at the
// MAJOR.MINOR level, an output.CodeTargetVersionMismatch warning is
// appended to Errors. Pass "" for targetVersion when the caller wants
// no target-version stamping (matches the CLI behaviour when
// --target-version is omitted).
//
// The append site here mirrors the CLI append site in
// cmd/newEnvelope; both must use output.VersionMismatchWarning so the
// two surfaces stay in sync.
func baseEnvelope(parserVersion, targetVersion string) output.Envelope {
	env := output.Envelope{
		Tier:             output.TierZeroConfig,
		ParserVersion:    parserVersion,
		ConnectionStatus: output.ConnectionDisconnected,
	}
	stampTargetVersion(&env, parserVersion, targetVersion)
	return env
}

// schemaFileEnvelope returns a pre-populated Envelope for Tier 2
// (schema_file, disconnected) tools — list_tables, describe_table,
// and validate_sql when given an inline schemas argument. targetVersion
// follows the same contract as baseEnvelope: empty means "no
// target-version stamping," non-empty stamps the field and (when
// MAJOR.MINOR diverges from parserVersion) appends a mismatch
// warning. Tools that do not yet accept the target_version argument
// (list_tables, describe_table) pass "" so they get today's behaviour
// unchanged; validate_sql passes its resolved value so a client
// supplying target_version alongside schemas still gets the stamping.
func schemaFileEnvelope(parserVersion, targetVersion string) output.Envelope {
	env := output.Envelope{
		Tier:             output.TierSchemaFile,
		ParserVersion:    parserVersion,
		ConnectionStatus: output.ConnectionDisconnected,
	}
	stampTargetVersion(&env, parserVersion, targetVersion)
	return env
}

// connectedEnvelope returns a pre-populated Envelope for Tier 3
// (connected) tools. ConnectionStatus starts disconnected and is flipped
// to connected by the handler after a successful round-trip to the
// cluster, so a partial failure surfaces with the correct state.
//
// targetVersion follows the same contract as baseEnvelope: empty
// means "no target-version stamping," non-empty stamps the field and
// (when MAJOR.MINOR diverges from parserVersion) appends a mismatch
// warning. This keeps Tier 3 tools consistent with the CLI and Tier 1
// surfaces.
func connectedEnvelope(parserVersion, targetVersion string) output.Envelope {
	env := output.Envelope{
		Tier:             output.TierConnected,
		ParserVersion:    parserVersion,
		ConnectionStatus: output.ConnectionDisconnected,
	}
	stampTargetVersion(&env, parserVersion, targetVersion)
	return env
}

// stampTargetVersion is the shared post-construction step for
// baseEnvelope and connectedEnvelope. Centralising it ensures the
// two surfaces never drift on whether the warning is appended or
// what code it carries.
func stampTargetVersion(env *output.Envelope, parserVersion, targetVersion string) {
	if targetVersion == "" {
		return
	}
	env.TargetVersion = targetVersion
	if warning, ok := output.VersionMismatchWarning(parserVersion, targetVersion); ok {
		env.Errors = append(env.Errors, warning)
	}
}

// resolveTargetVersion picks the target version to stamp onto the
// envelope for a single tool call. The per-call target_version
// argument wins over defaultTargetVersion when it is present, a
// string, and non-empty after trimming. A non-string value is a
// hard error (returned as a tool error) because there is no
// reasonable interpretation; an empty string is treated as "use the
// default" so clients can send the field unconditionally without
// disabling the server-level default.
//
// On success the returned *mcp.CallToolResult is nil. On a malformed
// per-call argument it is a pre-built tool error so the caller can
// return immediately rather than producing a misleading envelope.
func resolveTargetVersion(req mcp.CallToolRequest, defaultTargetVersion string) (string, *mcp.CallToolResult) {
	raw, exists := req.GetArguments()[TargetVersionParamName]
	if !exists {
		return defaultTargetVersion, nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", mcp.NewToolResultError(fmt.Sprintf(
			"%s parameter must be a string, got %T", TargetVersionParamName, raw))
	}
	s = strings.TrimSpace(s)
	if s == "" {
		// Treat an empty per-call value as "use the default" rather
		// than as a validation error; clients that send {"target_version": ""}
		// likely mean "no override".
		return defaultTargetVersion, nil
	}
	canonical, err := output.ValidateTargetVersion(s)
	if err != nil {
		return "", mcp.NewToolResultError(fmt.Sprintf(
			"%s parameter: %v", TargetVersionParamName, err))
	}
	return canonical, nil
}

// resolveSafetyMode picks the safety mode for a single Tier 3 tool
// call. Missing or empty mode arguments fall back to safety.DefaultMode
// (read_only). A non-string value is a hard error (tool-level) because
// there is no reasonable interpretation. An unknown mode token is also
// a tool-level error so the agent gets a precise "valid choices are…"
// message rather than a misleading safety violation.
//
// On success the returned *mcp.CallToolResult is nil. On any malformed
// argument it is a pre-built tool error.
func resolveSafetyMode(req mcp.CallToolRequest) (safety.Mode, *mcp.CallToolResult) {
	raw, exists := req.GetArguments()[ModeParamName]
	if !exists {
		return safety.DefaultMode, nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", mcp.NewToolResultError(fmt.Sprintf(
			"%s parameter must be a string, got %T", ModeParamName, raw))
	}
	m, err := safety.ParseMode(strings.TrimSpace(s))
	if err != nil {
		return "", mcp.NewToolResultError(fmt.Sprintf(
			"%s parameter: %v", ModeParamName, err))
	}
	return m, nil
}

// resolveStatementTimeout picks the statement_timeout for a single
// Tier 3 tool call. Missing or non-positive values fall back to
// conn.DefaultStatementTimeout (the same fallback WithStatementTimeout
// applies, repeated here so the resolved duration is observable to
// callers and tests). MCP delivers numeric arguments as float64; an
// integer-valued float is the canonical wire form for milliseconds.
// A non-numeric value is a tool-level error.
func resolveStatementTimeout(req mcp.CallToolRequest) (time.Duration, *mcp.CallToolResult) {
	raw, exists := req.GetArguments()[StatementTimeoutParamName]
	if !exists {
		return conn.DefaultStatementTimeout, nil
	}
	ms, ok := raw.(float64)
	if !ok {
		return 0, mcp.NewToolResultError(fmt.Sprintf(
			"%s parameter must be a number (milliseconds), got %T", StatementTimeoutParamName, raw))
	}
	if ms <= 0 {
		return conn.DefaultStatementTimeout, nil
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// envelopeResult marshals env as JSON and wraps it in a successful MCP
// tool result. If marshalling fails, it returns a tool error instead.
func envelopeResult(env output.Envelope) (*mcp.CallToolResult, error) {
	body, err := json.Marshal(env)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
	}
	return mcp.NewToolResultText(string(body)), nil
}
