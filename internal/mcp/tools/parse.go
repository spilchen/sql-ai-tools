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
	"github.com/spilchen/sql-ai-tools/internal/sqlparse"
	"github.com/spilchen/sql-ai-tools/internal/version"
)

// ParseSQLTool returns the MCP tool definition for parse_sql.
func ParseSQLTool() mcp.Tool {
	return mcp.NewTool(
		ParseSQLToolName,
		mcp.WithDescription("Parse and classify SQL statements. Returns an envelope with statement type (DDL/DML/DCL/TCL), tag, and original SQL for each statement in the input."),
		mcp.WithString("sql", mcp.Required(), mcp.Description("SQL string to parse (may contain multiple semicolon-separated statements)")),
		mcp.WithString(TargetVersionParamName, mcp.Description(TargetVersionParamDescription)),
	)
}

// ParseSQLHandler returns the handler for the parse_sql tool. It
// delegates to sqlparse.Classify and wraps the result in the standard
// output.Envelope used by all Tier 1 tools. defaultTargetVersion is
// the server-level default (typically the value of --target-version on
// the `crdb-sql mcp` invocation); per-call target_version arguments
// override it.
func ParseSQLHandler(parserVersion, defaultTargetVersion string) server.ToolHandlerFunc {
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

		parsed, err := parser.Parse(sql)
		if err != nil {
			env.Errors = append(env.Errors, diag.FromParseError(err, sql))
			return envelopeResult(env)
		}
		stmts := sqlparse.ClassifyParsed(parsed)
		env.Errors = append(env.Errors, version.Inspect(parsed, target, nil)...)

		data, err := json.Marshal(stmts)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("encode statements: %v", err)), nil
		}
		env.Data = data
		return envelopeResult(env)
	}
}
