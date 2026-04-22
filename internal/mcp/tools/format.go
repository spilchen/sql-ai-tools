// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/sqlformat"
)

// FormatSQLTool returns the MCP tool definition for format_sql.
func FormatSQLTool() mcp.Tool {
	return mcp.NewTool(
		FormatSQLToolName,
		mcp.WithDescription("Pretty-print SQL statements in canonical CockroachDB format. Returns an envelope with the formatted SQL string."),
		mcp.WithString("sql", mcp.Required(), mcp.Description("SQL string to format (may contain multiple semicolon-separated statements)")),
	)
}

// FormatSQLHandler returns the handler for the format_sql tool. It
// delegates to sqlformat.Format and wraps the result in the standard
// output.Envelope used by all Tier 1 tools.
func FormatSQLHandler(parserVersion string) server.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sql, toolErr := extractSQL(req)
		if toolErr != nil {
			return toolErr, nil
		}

		env := baseEnvelope(parserVersion)

		formatted, err := sqlformat.Format(sql)
		if err != nil {
			env.Errors = []output.Error{diag.FromParseError(err, sql)}
			return envelopeResult(env)
		}

		data, err := json.Marshal(struct {
			FormattedSQL string `json:"formatted_sql"`
		}{FormattedSQL: formatted})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
		}
		env.Data = data
		return envelopeResult(env)
	}
}
