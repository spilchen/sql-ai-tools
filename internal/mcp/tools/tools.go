// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package tools provides MCP tool handler constructors for the SQL
// tools. The Tier 1 (zero-config) tools are parse_sql, validate_sql,
// format_sql, and detect_risky_query; explain_sql is the only Tier 3
// (connected) tool, requiring a per-call DSN since the MCP server holds
// no per-session connection state.
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

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/spilchen/sql-ai-tools/internal/output"
)

// Registered MCP tool names.
const (
	ParseSQLToolName         = "parse_sql"
	ValidateSQLToolName      = "validate_sql"
	FormatSQLToolName        = "format_sql"
	DetectRiskyQueryToolName = "detect_risky_query"
	ExplainSQLToolName       = "explain_sql"
)

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
// for the common "sql" parameter, kept so the four SQL-only handlers
// (parse, validate, format, risk) stay terse.
func extractSQL(req mcp.CallToolRequest) (string, *mcp.CallToolResult) {
	return extractRequiredString(req, "sql")
}

// baseEnvelope returns a pre-populated Envelope for Tier 1 (zero-config,
// disconnected) tools.
func baseEnvelope(parserVersion string) output.Envelope {
	return output.Envelope{
		Tier:             output.TierZeroConfig,
		ParserVersion:    parserVersion,
		ConnectionStatus: output.ConnectionDisconnected,
	}
}

// connectedEnvelope returns a pre-populated Envelope for Tier 3
// (connected) tools. ConnectionStatus starts disconnected and is flipped
// to connected by the handler after a successful round-trip to the
// cluster, so a partial failure surfaces with the correct state.
func connectedEnvelope(parserVersion string) output.Envelope {
	return output.Envelope{
		Tier:             output.TierConnected,
		ParserVersion:    parserVersion,
		ConnectionStatus: output.ConnectionDisconnected,
	}
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
