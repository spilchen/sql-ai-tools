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
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/semcheck"
)

// ValidateSQLTool returns the MCP tool definition for validate_sql.
func ValidateSQLTool() mcp.Tool {
	return mcp.NewTool(
		ValidateSQLToolName,
		mcp.WithDescription("Validate SQL for syntax and type errors. Returns an envelope with {\"valid\": true} on success, or structured errors with SQLSTATE codes, severity, message, and source position on failure."),
		mcp.WithString("sql", mcp.Required(), mcp.Description("SQL string to validate")),
		mcp.WithString(TargetVersionParamName, mcp.Description(TargetVersionParamDescription)),
	)
}

// ValidateSQLHandler returns the handler for the validate_sql tool. It
// parses the SQL, runs expression type checking, and returns the
// standard output.Envelope used by all Tier 1 tools.
// defaultTargetVersion is the server-level default; per-call
// target_version arguments override it.
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

		env := baseEnvelope(parserVersion, target)

		stmts, parseErr := parser.Parse(sql)
		if parseErr != nil {
			env.Errors = []output.Error{diag.FromParseError(parseErr, sql)}
			return envelopeResult(env)
		}

		if typeErrs := semcheck.CheckExprTypes(stmts, sql); len(typeErrs) > 0 {
			env.Errors = typeErrs
			return envelopeResult(env)
		}

		data, err := json.Marshal(struct {
			Valid bool `json:"valid"`
		}{Valid: true})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
		}
		env.Data = data
		return envelopeResult(env)
	}
}
