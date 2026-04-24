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

	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/risk"
	"github.com/spilchen/sql-ai-tools/internal/version"
)

// DetectRiskySQLTool returns the MCP tool definition for detect_risky_sql.
func DetectRiskySQLTool() mcp.Tool {
	return mcp.NewTool(
		DetectRiskySQLToolName,
		mcp.WithDescription("Detect risky SQL patterns via AST walk (parser-only; no cluster contact, no statement execution). Flags issues such as DELETE/UPDATE without WHERE, DROP/TRUNCATE, SELECT *, SERIAL or missing primary keys, deep OFFSET pagination, and XA two-phase-commit statements. Returns findings with reason codes, severity, and fix hints. "+SharedParserBehaviorTag),
		mcp.WithString("sql", mcp.Required(), mcp.Description("SQL string to analyze for risky patterns")),
		mcp.WithString(TargetVersionParamName, mcp.Description(TargetVersionParamDescription)),
	)
}

// DetectRiskySQLHandler returns the handler for the detect_risky_sql
// tool. It delegates to risk.Analyze and wraps the result in the standard
// output.Envelope used by all tools in this package. defaultTargetVersion
// is the server-level default; per-call target_version arguments
// override it.
func DetectRiskySQLHandler(parserVersion, defaultTargetVersion string) server.ToolHandlerFunc {
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

		originalSQL := sql
		strip := preprocessSQL(&env, sql)
		sql = strip.Stripped

		before := len(env.Errors)
		parsed, err := parser.Parse(sql)
		if err != nil {
			env.Errors = append(env.Errors, diag.FromParseError(err, sql))
			translateErrorPositions(&env, before, originalSQL, strip)
			return envelopeResult(env)
		}
		env.Errors = append(env.Errors, version.Inspect(parsed, target, nil)...)
		findings := risk.AnalyzeParsed(parsed, sql)

		data, err := json.Marshal(findings)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("encode findings: %v", err)), nil
		}
		env.Data = data
		return envelopeResult(env)
	}
}
