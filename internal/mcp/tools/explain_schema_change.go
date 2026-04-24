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
	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/safety"
)

// ExplainSchemaChangeTool returns the MCP tool definition for
// explain_schema_change. This is the discoverable name for CRDB's
// `EXPLAIN (DDL, SHAPE)` capability — the underlying SQL syntax is
// buried in the manual, so the tool name lets agents reach for it by
// what it does (preview a schema-change plan) rather than by knowing
// the exact incantation.
//
// Like explain_sql, the `dsn` parameter is required because MCP
// sessions are stateless: the server holds no per-client connection,
// and credentials are never logged or echoed back.
func ExplainSchemaChangeTool() mcp.Tool {
	return mcp.NewTool(
		ExplainSchemaChangeToolName,
		mcp.WithDescription(`Run EXPLAIN (DDL, SHAPE) against a CockroachDB cluster and return the declarative schema-changer plan as structured JSON. The wrapped DDL is not executed — the schema changer only compiles a plan. Returns the operations list (with backfill / merge / validate steps), the canonicalized statement, and the raw text the cluster returned.`),
		mcp.WithString("sql", mcp.Required(), mcp.Description("DDL statement to plan (e.g. ALTER TABLE ... ADD COLUMN ...)")),
		mcp.WithString("dsn", mcp.Required(), mcp.Description("CockroachDB connection string (postgres:// URI). For TLS-only clusters, supply sslmode/sslrootcert/sslcert/sslkey either as URI query params or as the matching top-level fields below.")),
		mcp.WithString(TargetVersionParamName, mcp.Description(TargetVersionParamDescription)),
		mcp.WithString(ModeParamName, mcp.Description(ModeParamDescription)),
		mcp.WithNumber(StatementTimeoutParamName, mcp.Description(StatementTimeoutParamDescription)),
		mcp.WithString(SSLModeParamName, mcp.Description(SSLModeParamDescription)),
		mcp.WithString(SSLRootCertParamName, mcp.Description(SSLRootCertParamDescription)),
		mcp.WithString(SSLCertParamName, mcp.Description(SSLCertParamDescription)),
		mcp.WithString(SSLKeyParamName, mcp.Description(SSLKeyParamDescription)),
	)
}

// ExplainSchemaChangeHandler returns the handler for the
// explain_schema_change tool. The envelope's ConnectionStatus starts
// disconnected and flips to connected only after a successful run, so
// partial-failure envelopes report the actual reached state.
// Cluster-side errors (timeouts, syntax in the wrapped DDL, perm
// denied) populate env.Errors via diag.FromClusterError to carry
// SQLSTATE; tool-level errors (missing parameters) are returned as
// mcp.NewToolResultError per the discipline documented in tools.go.
//
// defaultTargetVersion is the server-level default; per-call
// target_version arguments override it.
func ExplainSchemaChangeHandler(parserVersion, defaultTargetVersion string) server.ToolHandlerFunc {
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
		mode, toolErr := resolveSafetyMode(req)
		if toolErr != nil {
			return toolErr, nil
		}
		timeout, toolErr := resolveStatementTimeout(req)
		if toolErr != nil {
			return toolErr, nil
		}

		env := connectedEnvelope(parserVersion, target)

		violation, err := safety.Check(mode, safety.OpExplainDDL, sql)
		if err != nil {
			env.Errors = []output.Error{diag.FromParseError(err, sql)}
			return envelopeResult(env)
		}
		if violation != nil {
			env.Errors = []output.Error{safety.Envelope(violation)}
			return envelopeResult(env)
		}

		mergedDSN, toolErr := mergeDSNWithTLS(req, &env, dsn)
		if toolErr != nil {
			return toolErr, nil
		}

		mgr := conn.NewManager(mergedDSN, conn.WithStatementTimeout(timeout))
		defer mgr.Close(ctx) //nolint:errcheck // best-effort cleanup

		result, err := mgr.ExplainDDL(ctx, sql)
		if err != nil {
			env.Errors = []output.Error{diag.FromClusterError(err, sql)}
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
