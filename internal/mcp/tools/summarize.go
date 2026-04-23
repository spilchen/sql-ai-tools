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
	"github.com/spilchen/sql-ai-tools/internal/summarize"
)

// SummarizeSQLTool returns the MCP tool definition for summarize_sql.
func SummarizeSQLTool() mcp.Tool {
	return mcp.NewTool(
		SummarizeSQLToolName,
		mcp.WithDescription("Summarize SQL statements via AST walk. Returns per-statement operation, tables, predicates, joins, affected_columns (DML write set), referenced_columns (full read+write footprint), select_star (true when projection uses '*' or 't.*' so referenced_columns is a lower bound), and risk level."),
		mcp.WithString("sql", mcp.Required(), mcp.Description("SQL string to summarize")),
		mcp.WithString(TargetVersionParamName, mcp.Description(TargetVersionParamDescription)),
	)
}

// SummarizeSQLHandler returns the handler for the summarize_sql tool.
// It delegates to summarize.Summarize and wraps the result in the
// standard output.Envelope used by all tools in this package.
// defaultTargetVersion is the server-level default; per-call
// target_version arguments override it.
func SummarizeSQLHandler(parserVersion, defaultTargetVersion string) server.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sql, toolErr := extractSQL(req)
		if toolErr != nil {
			return toolErr, nil
		}
		target, toolErr := resolveTargetVersion(req, defaultTargetVersion)
		if toolErr != nil {
			return toolErr, nil
		}

		env := baseEnvelope(parserVersion, target)

		summaries, err := summarize.Summarize(sql)
		if err != nil {
			env.Errors = []output.Error{diag.FromParseError(err, sql)}
			return envelopeResult(env)
		}

		data, err := json.Marshal(summaries)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("encode summaries: %v", err)), nil
		}
		env.Data = data
		return envelopeResult(env)
	}
}
