// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package tools provides MCP tool handler constructors for the Tier 1
// SQL tools (parse_sql, validate_sql, format_sql). Each handler returns
// the same output.Envelope JSON shape that the CLI emits under
// --output=json, so MCP clients get structured errors, parser version,
// and tier metadata consistent with the CLI surface.
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

// Registered MCP tool names for the Tier 1 SQL tools.
const (
	ParseSQLToolName    = "parse_sql"
	ValidateSQLToolName = "validate_sql"
	FormatSQLToolName   = "format_sql"
)

// extractSQL validates and returns the required "sql" string parameter
// from an MCP tool request. Leading and trailing whitespace is trimmed
// so all handlers behave consistently. On success, the returned
// *mcp.CallToolResult is nil. On failure (missing, wrong type, empty),
// it is a pre-built tool error that the caller should return immediately.
func extractSQL(req mcp.CallToolRequest) (string, *mcp.CallToolResult) {
	raw, exists := req.GetArguments()["sql"]
	if !exists {
		return "", mcp.NewToolResultError("sql parameter is required")
	}
	sql, ok := raw.(string)
	if !ok {
		return "", mcp.NewToolResultError(fmt.Sprintf("sql parameter must be a string, got %T", raw))
	}
	sql = strings.TrimSpace(sql)
	if sql == "" {
		return "", mcp.NewToolResultError("sql parameter must not be empty")
	}
	return sql, nil
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

// envelopeResult marshals env as JSON and wraps it in a successful MCP
// tool result. If marshalling fails, it returns a tool error instead.
func envelopeResult(env output.Envelope) (*mcp.CallToolResult, error) {
	body, err := json.Marshal(env)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
	}
	return mcp.NewToolResultText(string(body)), nil
}
