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

// SimulateSQLTool returns the MCP tool definition for simulate_sql.
// The dispatcher behind this tool routes each parsed statement to a
// non-executing EXPLAIN flavor (EXPLAIN ANALYZE for SELECT, plain
// EXPLAIN for DML writes, EXPLAIN (DDL, SHAPE) for DDL) and returns
// per-statement plan + (for DDL) row-count annotations. The `dsn`
// parameter is required because MCP sessions are stateless: the
// server has no per-client connection to reuse, so each call carries
// the connection string. Credentials are never logged or echoed back.
func SimulateSQLTool() mcp.Tool {
	return mcp.NewTool(
		SimulateSQLToolName,
		mcp.WithDescription("Simulate one or more SQL statements without applying them. Each parsed statement is dispatched to a non-executing EXPLAIN flavor: SELECT runs through EXPLAIN ANALYZE (real runtime stats; reads have no side effects), INSERT/UPDATE/DELETE/UPSERT runs through plain EXPLAIN (planner estimates only — the write is never applied), and DDL runs through EXPLAIN (DDL, SHAPE) plus a SHOW STATISTICS row-count annotation per affected table. Multi-statement input returns one entry per statement in parse order. Tolerates cockroach sql REPL paste artifacts (leading `root@host:port/db>` prompt and `-> ` continuation prompts). Pass raw paste in one shot; do not pre-strip."),
		mcp.WithString("sql", mcp.Required(), mcp.Description("SQL statement(s) to simulate. Multi-statement input is split per ';' and each statement is dispatched independently.")),
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

// SimulateSQLHandler returns the handler for the simulate_sql tool.
// The envelope's ConnectionStatus starts disconnected and flips to
// connected only after a successful Simulate call, so partial-failure
// envelopes report the actual reached state. Per-statement failures
// (cluster reject, statement timeout, missing target table for stats)
// land on the corresponding SimulateStep.Error rather than aborting
// the whole call; method-level errors (parse failure, initial
// connect) populate env.Errors via diag.FromClusterError.
//
// defaultTargetVersion is the server-level default; per-call
// target_version arguments override it. The resolved value is
// stamped onto the envelope (and may add a mismatch warning) per
// the contract documented on connectedEnvelope.
func SimulateSQLHandler(parserVersion, defaultTargetVersion string) server.ToolHandlerFunc {
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
		safetyBefore := len(env.Errors)
		violation, err := safety.Check(mode, safety.OpSimulate, sql)
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
		result, err := mgr.Simulate(ctx, sql)
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

		// Promote per-step failures into an envelope-level warning
		// so agents that read only the top-level errors[] still see
		// the partial failure. Without this, a multi-statement
		// simulation where step 2 of 5 failed would render as a
		// fully successful tool call with the failure buried
		// inside data.steps. The index slices land in Context so
		// agents can retry only the failed steps without parsing
		// the human-readable message.
		if msg, planFails, statsFails, ok := result.StepFailureSummary(); ok {
			ctx := make(map[string]any, 2)
			if len(planFails) > 0 {
				ctx["plan_failed_steps"] = planFails
			}
			if len(statsFails) > 0 {
				ctx["stats_failed_steps"] = statsFails
			}
			env.Errors = append(env.Errors, output.Error{
				Code:     "simulate_step_failure",
				Severity: output.SeverityError,
				Message:  msg,
				Category: "simulate",
				Context:  ctx,
			})
		}

		return envelopeResult(env)
	}
}
