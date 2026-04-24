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
	"github.com/spilchen/sql-ai-tools/internal/sqlformat"
	"github.com/spilchen/sql-ai-tools/internal/version"
)

// FormatSQLTool returns the MCP tool definition for format_sql.
func FormatSQLTool() mcp.Tool {
	return mcp.NewTool(
		FormatSQLToolName,
		mcp.WithDescription("Pretty-print SQL statements in canonical CockroachDB format (parser-only; no cluster contact). Returns an envelope with the formatted SQL string. Use for human-readable display in messages back to the user, or as a normalization step when comparing two SQL strings for semantic equivalence. For literal-redacted fingerprinting, prefer parse_sql (returns a normalized form). "+SharedParserBehaviorTag),
		mcp.WithString("sql", mcp.Required(), mcp.Description("SQL string to format (may contain multiple semicolon-separated statements)")),
		mcp.WithString(TargetVersionParamName, mcp.Description(TargetVersionParamDescription)),
	)
}

// FormatSQLHandler returns the handler for the format_sql tool. It
// delegates to sqlformat.Format and wraps the result in the standard
// output.Envelope used by all Tier 1 tools. defaultTargetVersion is
// the server-level default; per-call target_version arguments override
// it.
func FormatSQLHandler(parserVersion, defaultTargetVersion string) server.ToolHandlerFunc {
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

		// Auto-clean cockroach sql REPL prompts the same way the CLI
		// format subcommand does, so MCP clients pasting transcripts
		// get the same forgiving behavior. preprocessSQL also surfaces
		// the input_preprocessed warning when stripping fired, so the
		// caller can see the modification.
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

		formatted, err := sqlformat.FormatParsed(parsed)
		if err != nil {
			// Surface pretty-printer failures through the envelope (not
			// mcp.NewToolResultError) so any version.Inspect warnings
			// already appended above survive into the response. Without
			// this, an opt-in target_version warning would be silently
			// dropped the moment cfg.Pretty hiccuped — exactly the
			// regression this tool was wired to prevent.
			env.Errors = append(env.Errors, output.Error{
				Code:     "internal_error",
				Severity: output.SeverityError,
				Message:  fmt.Sprintf("format: %v", err),
			})
			return envelopeResult(env)
		}

		data, err := json.Marshal(struct {
			FormattedSQL string `json:"formatted_sql"`
		}{FormattedSQL: formatted})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
		}
		env.Data = data
		return envelopeResult(env)
	}
}
