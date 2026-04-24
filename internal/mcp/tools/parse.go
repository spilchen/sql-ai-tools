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
		mcp.WithDescription("Parse and classify SQL statements (parser-only; no cluster contact). For each statement returns: statement_type (DDL/DML/DCL/TCL), tag (e.g. SELECT, INSERT), the original sql, and a normalized form with literal constants redacted to _ (suitable as a fingerprint for dedup, query-log grouping, or cache keys — structurally identical queries with different constants share one normalized form). Use when you need to classify or fingerprint SQL; for type and name correctness use validate_sql. "+SharedParserBehaviorTag),
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
