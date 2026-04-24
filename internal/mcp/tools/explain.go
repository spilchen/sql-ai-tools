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

// ExplainSQLTool returns the MCP tool definition for explain_sql. The
// `dsn` parameter is required because MCP sessions are stateless: the
// server has no per-client connection to reuse, so each call carries
// the connection string. Credentials are never logged or echoed back.
func ExplainSQLTool() mcp.Tool {
	return mcp.NewTool(
		ExplainSQLToolName,
		mcp.WithDescription("Plan a single SQL statement against a CockroachDB cluster without executing it. Auto-dispatches by statement class: SELECT/DML through plain `EXPLAIN`, DDL through `EXPLAIN (DDL, SHAPE)`. Returns a discriminated JSON result keyed by `strategy` — `explain` carries the operator tree + header + raw rows; `explain_ddl` carries the schema-changer operations list + canonicalized statement + raw text. The cheap default Tier-3 entry point: escalate to `simulate_sql` only when you need measured runtime stats (EXPLAIN ANALYZE) or per-table SHOW STATISTICS row counts on a DDL plan. Syntax errors include \"did you mean?\" suggestions when the offending token resembles a SQL keyword. Tolerates cockroach sql REPL paste artifacts (leading `root@host:port/db>` prompt and `-> ` continuation prompts). Pass raw paste in one shot; do not pre-strip."),
		mcp.WithString("sql", mcp.Required(), mcp.Description("SQL statement to plan (SELECT, DML, or DDL — auto-dispatched to the right EXPLAIN flavor)")),
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
		mode, toolErr := resolveSafetyMode(req)
		if toolErr != nil {
			return toolErr, nil
		}
		timeout, toolErr := resolveStatementTimeout(req)
		if toolErr != nil {
			return toolErr, nil
		}

		env := connectedEnvelope(parserVersion, target)

		originalSQL := sql
		strip := preprocessSQL(&env, sql)
		sql = strip.Stripped

		// Safety check runs before any cluster contact: a rejection
		// surfaces in env.Errors with connection_status=disconnected,
		// matching the CLI's behaviour. Parse errors propagate via
		// diag.FromParseError so the agent gets the SQLSTATE-tagged
		// syntax diagnostic, not a misleading safety violation.
		//
		// Note: safety.Check / safety.Envelope-built diagnostics carry
		// no Position field today, so translation is a no-op for the
		// safety-rejection branch — but we run it anyway to stay
		// future-proof if safety later attaches positions.
		safetyBefore := len(env.Errors)
		violation, err := safety.Check(mode, safety.OpExplain, sql)
		if err != nil {
			env.Errors = append(env.Errors, diag.FromParseError(err, sql))
			translateErrorPositions(&env, safetyBefore, originalSQL, strip)
			return envelopeResult(env)
		}
		if violation != nil {
			env.Errors = append(env.Errors, safety.Envelope(violation))
			translateErrorPositions(&env, safetyBefore, originalSQL, strip)
			return envelopeResult(env)
		}

		mergedDSN, toolErr := mergeDSNWithTLS(req, &env, dsn)
		if toolErr != nil {
			return toolErr, nil
		}

		mgr := conn.NewManager(mergedDSN, conn.WithStatementTimeout(timeout))
		defer mgr.Close(ctx) //nolint:errcheck // best-effort cleanup

		clusterBefore := len(env.Errors)
		result, err := mgr.ExplainAny(ctx, sql)
		if err != nil {
			env.Errors = append(env.Errors, diag.FromClusterError(err, sql))
			translateErrorPositions(&env, clusterBefore, originalSQL, strip)
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
