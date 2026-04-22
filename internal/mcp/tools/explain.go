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

	"github.com/spilchen/sql-ai-tools/internal/conn"
	"github.com/spilchen/sql-ai-tools/internal/output"
)

// ExplainSQLTool returns the MCP tool definition for explain_sql. The
// `dsn` parameter is required because MCP sessions are stateless: the
// server has no per-client connection to reuse, so each call carries
// the connection string. Credentials are never logged or echoed back.
func ExplainSQLTool() mcp.Tool {
	return mcp.NewTool(
		ExplainSQLToolName,
		mcp.WithDescription(`Run EXPLAIN against a CockroachDB cluster and return the plan as structured JSON. The wrapped statement is not executed (this is plain EXPLAIN, not EXPLAIN ANALYZE). Returns the operator tree, header (distribution/vectorized), and the raw tabular rows.`),
		mcp.WithString("sql", mcp.Required(), mcp.Description("SQL DML statement to explain")),
		mcp.WithString("dsn", mcp.Required(), mcp.Description("CockroachDB connection string (postgres:// URI)")),
		mcp.WithString(TargetVersionParamName, mcp.Description(TargetVersionParamDescription)),
	)
}

// ExplainSQLHandler returns the handler for the explain_sql tool. The
// envelope's ConnectionStatus starts disconnected and flips to connected
// only after a successful EXPLAIN, so partial-failure envelopes report
// the actual reached state. Cluster-side errors (timeouts, syntax in the
// wrapped statement, perm denied) populate env.Errors; tool-level errors
// (missing parameters) are returned as mcp.NewToolResultError per the
// discipline documented in tools.go.
//
// defaultTargetVersion is the server-level default; per-call
// target_version arguments override it. The resolved value is
// stamped onto the envelope (and may add a mismatch warning) per
// the contract documented on connectedEnvelope.
func ExplainSQLHandler(parserVersion, defaultTargetVersion string) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sql, toolErr := extractRequiredString(req, "sql")
		if toolErr != nil {
			return toolErr, nil
		}
		dsn, toolErr := extractRequiredString(req, "dsn")
		if toolErr != nil {
			return toolErr, nil
		}
		target, toolErr := resolveTargetVersion(req, defaultTargetVersion)
		if toolErr != nil {
			return toolErr, nil
		}

		env := connectedEnvelope(parserVersion, target)

		mgr := conn.NewManager(dsn)
		defer mgr.Close(ctx) //nolint:errcheck // best-effort cleanup

		result, err := mgr.Explain(ctx, sql)
		if err != nil {
			env.Errors = []output.Error{{
				Code:     "internal_error",
				Severity: output.SeverityError,
				Message:  err.Error(),
			}}
			return envelopeResult(env)
		}

		env.ConnectionStatus = output.ConnectionConnected
		data, err := json.Marshal(result)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
		}
		env.Data = data
		return envelopeResult(env)
	}
}
