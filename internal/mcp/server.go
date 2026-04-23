// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package mcp builds the crdb-sql Model Context Protocol server.
//
// The server registers a health-check tool (ping), five Tier 1 SQL
// tools (parse_sql, validate_sql, format_sql, detect_risky_query,
// summarize_sql), two Tier 2 catalog tools (list_tables,
// describe_table) that operate on inline CREATE TABLE schemas, and
// two Tier 3 connected tools (explain_sql, explain_schema_change)
// that run against a live cluster. validate_sql is dual-tier: it
// runs Tier 1 by default and lifts to Tier 2 (name resolution) when
// the caller supplies inline schemas. Keeping construction pure (no
// transport, no I/O) lets the cmd layer pick a transport — currently
// just stdio — and lets tests exercise individual tool handlers
// directly.
//
// Versions are passed in by the caller rather than read from
// debug.ReadBuildInfo here, so this package stays free of
// build-info plumbing. The cmd/version.go helpers own that
// resolution and feed the resolved strings to NewServer.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/spilchen/sql-ai-tools/internal/mcp/tools"
)

// PingToolName is the registered MCP tool name for the health-check tool.
// All other tool name constants live in the tools subpackage.
const PingToolName = "ping"

// NewServer constructs an MCP server for crdb-sql. The three string
// arguments name distinct concepts that flow through every tool
// response, and callers must resolve them before invoking NewServer:
//
//   - crdbSQLVersion is the crdb-sql binary version (typically
//     cmd.Version). Reported in the MCP server handshake so clients
//     can identify which build they are talking to.
//   - parserVersion is the resolved cockroachdb-parser module version
//     (typically the result of cmd.parserVersion). Stamped into every
//     tool's envelope so clients always know which SQL dialect this
//     binary actually understands.
//   - defaultTargetVersion is the user-declared CockroachDB target
//     version (typically state.targetVersion from the --target-version
//     flag), or "" when the user did not supply one. Used as a default
//     for every tool call; per-call target_version arguments override
//     it.
//
// The returned server has no transport bound; callers wire it to stdio
// (or, in the future, sse/http) themselves.
func NewServer(crdbSQLVersion, parserVersion, defaultTargetVersion string) *server.MCPServer {
	s := server.NewMCPServer(
		"crdb-sql",
		crdbSQLVersion,
		server.WithToolCapabilities(false /* listChanged */),
	)
	s.AddTool(
		mcp.NewTool(
			PingToolName,
			mcp.WithDescription(`Health check. Returns {"ok": true, "parser_version": "<v>"} so clients can confirm the server is alive and see which cockroachdb-parser version it was built against.`),
		),
		pingHandler(parserVersion),
	)
	s.AddTool(tools.ParseSQLTool(), tools.ParseSQLHandler(parserVersion, defaultTargetVersion))
	s.AddTool(tools.ValidateSQLTool(), tools.ValidateSQLHandler(parserVersion, defaultTargetVersion))
	s.AddTool(tools.FormatSQLTool(), tools.FormatSQLHandler(parserVersion, defaultTargetVersion))
	s.AddTool(tools.DetectRiskyQueryTool(), tools.DetectRiskyQueryHandler(parserVersion, defaultTargetVersion))
	s.AddTool(tools.SummarizeSQLTool(), tools.SummarizeSQLHandler(parserVersion, defaultTargetVersion))
	s.AddTool(tools.ExplainSQLTool(), tools.ExplainSQLHandler(parserVersion, defaultTargetVersion))
	s.AddTool(tools.ExplainSchemaChangeTool(), tools.ExplainSchemaChangeHandler(parserVersion, defaultTargetVersion))
	s.AddTool(tools.ListTablesTool(), tools.ListTablesHandler(parserVersion))
	s.AddTool(tools.DescribeTableTool(), tools.DescribeTableHandler(parserVersion))
	return s
}

// pingHandler returns the handler for the `ping` tool. The parser
// version is captured at construction time and embedded in every
// response, so a single server instance always reports a stable
// version for the lifetime of the process.
func pingHandler(parserVersion string) server.ToolHandlerFunc {
	return func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		payload := pingResult{OK: true, ParserVersion: parserVersion}
		body, err := json.Marshal(payload)
		if err != nil {
			// json.Marshal of a struct with only string/bool fields cannot
			// fail in practice, but surface any future regression as a
			// tool-level error rather than a panic.
			return mcp.NewToolResultError(fmt.Sprintf("encode ping result: %v", err)), nil
		}
		return mcp.NewToolResultText(string(body)), nil
	}
}

// pingResult is the JSON shape returned by the `ping` tool. Field tags
// are the contract: clients (including Claude Code) read `ok` and
// `parser_version` by name, so renames here are breaking changes.
type pingResult struct {
	OK            bool   `json:"ok"`
	ParserVersion string `json:"parser_version"`
}
