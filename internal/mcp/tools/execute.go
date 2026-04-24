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

	"github.com/spilchen/sql-ai-tools/internal/conn"
	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/safety"
	"github.com/spilchen/sql-ai-tools/internal/version"
)

// defaultExecuteMaxRows is the row cap applied to read_only SELECTs
// when the caller does not override max_rows. Mirrors the CLI's
// --max-rows default so the two surfaces produce identical bounded
// results for the same input.
const defaultExecuteMaxRows = 1000

// ExecuteSQLTool returns the MCP tool definition for execute_sql.
// Like explain_sql, the dsn parameter is required because MCP
// sessions are stateless. The mode/timeout/max_rows knobs all carry
// the same semantics as the CLI's --mode/--timeout/--max-rows flags.
func ExecuteSQLTool() mcp.Tool {
	return mcp.NewTool(
		ExecuteSQLToolName,
		mcp.WithDescription("Execute SQL against a CockroachDB cluster with safety guardrails. Returns rows, columns, and the command tag in a structured envelope. The mode parameter selects the safety policy: read_only (default) admits non-mutating statements only; safe_write also admits INSERT/UPDATE/DELETE; full_access admits any parsed statement. For read_only SELECTs without a LIMIT, max_rows is injected so the cluster does not stream an unbounded result. Tolerates cockroach sql REPL paste artifacts (leading `root@host:port/db>` prompt and `-> ` continuation prompts). Pass raw paste in one shot; do not pre-strip."),
		mcp.WithString("sql", mcp.Required(), mcp.Description("SQL statement to execute")),
		mcp.WithString("dsn", mcp.Required(), mcp.Description("CockroachDB connection string (postgres:// URI). For TLS-only clusters, supply sslmode/sslrootcert/sslcert/sslkey either as URI query params or as the matching top-level fields below.")),
		mcp.WithString(TargetVersionParamName, mcp.Description(TargetVersionParamDescription)),
		mcp.WithString(ModeParamName, mcp.Description(ModeParamDescription)),
		mcp.WithNumber(StatementTimeoutParamName, mcp.Description(StatementTimeoutParamDescription)),
		mcp.WithNumber(MaxRowsParamName, mcp.Description(MaxRowsParamDescription)),
		mcp.WithString(SSLModeParamName, mcp.Description(SSLModeParamDescription)),
		mcp.WithString(SSLRootCertParamName, mcp.Description(SSLRootCertParamDescription)),
		mcp.WithString(SSLCertParamName, mcp.Description(SSLCertParamDescription)),
		mcp.WithString(SSLKeyParamName, mcp.Description(SSLKeyParamDescription)),
	)
}

// ExecuteSQLHandler returns the handler for the execute_sql tool. The
// envelope's ConnectionStatus starts disconnected and flips to
// connected only after a successful Execute, so partial-failure
// envelopes report the actual reached state. Cluster-side errors
// (timeouts, syntax errors, perm denied) populate env.Errors via
// diag.FromClusterError; tool-level errors (missing parameters) come
// back as mcp.NewToolResultError per the discipline in tools.go.
func ExecuteSQLHandler(parserVersion, defaultTargetVersion string) server.ToolHandlerFunc {
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
		maxRows, toolErr := resolveMaxRows(req, defaultExecuteMaxRows)
		if toolErr != nil {
			return toolErr, nil
		}

		env := connectedEnvelope(parserVersion, target)

		originalSQL := sql
		strip := preprocessSQL(&env, sql)
		sql = strip.Stripped

		// Parse once up front so version.Inspect, safety.CheckParsed,
		// and safety.MaybeInjectLimitParsed share a single AST. Append
		// (not assign) into env.Errors so a pre-stamped warning from
		// connectedEnvelope (e.g. target_version_mismatch) or
		// preprocessSQL (input_preprocessed) survives a downstream
		// parse / safety / cluster failure.
		//
		// Order matters: version.Inspect and safety.CheckParsed are
		// read-only walks; safety.MaybeInjectLimitParsed mutates
		// stmts[0].AST.Limit in place when injection fires, so the
		// inspectors must run first.
		parseBefore := len(env.Errors)
		parsed, err := parser.Parse(sql)
		if err != nil {
			env.Errors = append(env.Errors, diag.FromParseError(err, sql))
			translateErrorPositions(&env, parseBefore, originalSQL, strip)
			return envelopeResult(env)
		}
		env.Errors = append(env.Errors, version.Inspect(parsed, target, nil)...)

		safetyBefore := len(env.Errors)
		if violation := safety.CheckParsed(mode, safety.OpExecute, parsed); violation != nil {
			env.Errors = append(env.Errors, safety.Envelope(violation))
			// safety.Envelope carries no Position today, so translate is
			// a no-op — but run it to stay future-proof if safety later
			// attaches positions, matching the pattern in explain.go,
			// explain_schema_change.go, and simulate.go.
			translateErrorPositions(&env, safetyBefore, originalSQL, strip)
			return envelopeResult(env)
		}

		// LIMIT injection is scoped to read_only because the other
		// modes are explicit opt-ins where the user has already
		// accepted writes / unbounded scans.
		rewritten := sql
		var injected bool
		if mode == safety.ModeReadOnly && maxRows > 0 {
			if rw, did := safety.MaybeInjectLimitParsed(parsed, maxRows); did {
				rewritten = rw
				injected = true
			}
		}

		mergedDSN, toolErr := mergeDSNWithTLS(req, &env, dsn)
		if toolErr != nil {
			return toolErr, nil
		}

		mgr := conn.NewManager(mergedDSN, conn.WithStatementTimeout(timeout))
		defer mgr.Close(ctx) //nolint:errcheck // best-effort cleanup

		clusterBefore := len(env.Errors)
		result, err := mgr.Execute(ctx, rewritten, conn.ExecuteOptions{
			Mode:    mode,
			MaxRows: maxRows,
		})
		if err != nil {
			clusterErr := diag.FromClusterError(err, rewritten)
			// When LIMIT injection fires, rewritten is the canonicalized
			// AST re-serialized by tree.AsStringWithFlags — not stripped
			// SQL with an appended clause. Pgwire positions index into
			// rewritten, so strip.Translate (which maps stripped offsets
			// to original) cannot honestly translate them. Drop the
			// Position rather than report a confidently-wrong line/column.
			if injected {
				clusterErr.Position = nil
				if clusterErr.Context == nil {
					clusterErr.Context = make(map[string]any, 1)
				}
				clusterErr.Context[output.ContextPositionOmittedReason] = output.ReasonLimitInjectionRewroteSQL
			}
			env.Errors = append(env.Errors, clusterErr)
			// Translate any other appended errors (none today, but the
			// loop is the right place for future diagnostics that share
			// the stripped frame). The injected-cluster-error case above
			// has already nilled its Position so the translate is a no-op
			// for it.
			translateErrorPositions(&env, clusterBefore, originalSQL, strip)
			return envelopeResult(env)
		}

		if injected {
			limit := maxRows
			result.LimitInjected = &limit
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
