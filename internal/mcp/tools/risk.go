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
	"github.com/spilchen/sql-ai-tools/internal/risk"
)

// DetectRiskyQueryTool returns the MCP tool definition for detect_risky_query.
func DetectRiskyQueryTool() mcp.Tool {
	return mcp.NewTool(
		DetectRiskyQueryToolName,
		mcp.WithDescription("Detect risky SQL patterns such as DELETE/UPDATE without WHERE, DROP TABLE, and SELECT *. Returns findings with reason codes, severity, and fix hints."),
		mcp.WithString("sql", mcp.Required(), mcp.Description("SQL string to analyze for risky patterns")),
		mcp.WithString(TargetVersionParamName, mcp.Description(TargetVersionParamDescription)),
	)
}

// DetectRiskyQueryHandler returns the handler for the detect_risky_query
// tool. It delegates to risk.Analyze and wraps the result in the standard
// output.Envelope used by all tools in this package. defaultTargetVersion
// is the server-level default; per-call target_version arguments
// override it.
func DetectRiskyQueryHandler(parserVersion, defaultTargetVersion string) server.ToolHandlerFunc {
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

		findings, err := risk.Analyze(sql)
		if err != nil {
			env.Errors = []output.Error{diag.FromParseError(err, sql)}
			return envelopeResult(env)
		}

		data, err := json.Marshal(findings)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("encode findings: %v", err)), nil
		}
		env.Data = data
		return envelopeResult(env)
	}
}
