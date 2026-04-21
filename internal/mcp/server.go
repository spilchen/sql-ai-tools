// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package mcp builds the crdb-sql Model Context Protocol server.
//
// The server registers a health-check tool (ping), a SQL
// classification tool (parse_sql), and a risk-detection tool
// (detect_risky_query); future issues will add more SQL-aware
// tools (validate_sql, format_sql, …) to the same NewServer
// constructor. Keeping construction pure (no transport,
// no I/O) lets the cmd layer pick a transport — currently just
// stdio — and lets tests exercise individual tool handlers directly.
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

	"github.com/spilchen/sql-ai-tools/internal/risk"
	"github.com/spilchen/sql-ai-tools/internal/sqlparse"
)

// Tool name constants are exposed so tests and docs reference a single
// source of truth.
const (
	PingToolName             = "ping"
	ParseSQLToolName         = "parse_sql"
	DetectRiskyQueryToolName = "detect_risky_query"
)

// NewServer constructs an MCP server for crdb-sql. binaryVersion is the
// crdb-sql binary version string (typically cmd.Version), and
// parserVersion is the resolved cockroachdb-parser module version
// (typically the result of cmd.parserVersion). Both are reported
// verbatim — by the server handshake (binaryVersion) and by the `ping`
// tool (parserVersion) — so callers should resolve them before invoking
// NewServer.
//
// The returned server has no transport bound; callers wire it to stdio
// (or, in the future, sse/http) themselves.
func NewServer(binaryVersion, parserVersion string) *server.MCPServer {
	s := server.NewMCPServer(
		"crdb-sql",
		binaryVersion,
		server.WithToolCapabilities(false /* listChanged */),
	)
	s.AddTool(
		mcp.NewTool(
			PingToolName,
			mcp.WithDescription(`Health check. Returns {"ok": true, "parser_version": "<v>"} so clients can confirm the server is alive and see which cockroachdb-parser version it was built against.`),
		),
		pingHandler(parserVersion),
	)
	s.AddTool(
		mcp.NewTool(
			ParseSQLToolName,
			mcp.WithDescription("Parse and classify SQL statements. Returns statement type (DDL/DML/DCL/TCL), tag, and original SQL for each statement in the input."),
			mcp.WithString("sql", mcp.Required(), mcp.Description("SQL string to parse (may contain multiple semicolon-separated statements)")),
		),
		parseSQLHandler(parserVersion),
	)
	s.AddTool(
		mcp.NewTool(
			DetectRiskyQueryToolName,
			mcp.WithDescription("Detect risky SQL patterns such as DELETE/UPDATE without WHERE, DROP TABLE, and SELECT *. Returns findings with reason codes, severity, and fix hints."),
			mcp.WithString("sql", mcp.Required(), mcp.Description("SQL string to analyze for risky patterns")),
		),
		detectRiskyQueryHandler(parserVersion),
	)
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

// parseSQLHandler returns the handler for the `parse_sql` tool. It
// delegates to sqlparse.Classify for the actual parsing, so the MCP
// and CLI layers share one implementation.
func parseSQLHandler(parserVersion string) server.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		raw, exists := req.GetArguments()["sql"]
		if !exists {
			return mcp.NewToolResultError("sql parameter is required"), nil
		}
		sql, ok := raw.(string)
		if !ok {
			return mcp.NewToolResultError(fmt.Sprintf("sql parameter must be a string, got %T", raw)), nil
		}
		if sql == "" {
			return mcp.NewToolResultError("sql parameter must not be empty"), nil
		}

		stmts, err := sqlparse.Classify(sql)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("parse error: %v", err)), nil
		}

		result := parseSQLResult{
			ParserVersion: parserVersion,
			Statements:    stmts,
		}
		body, err := json.Marshal(result)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
		}
		return mcp.NewToolResultText(string(body)), nil
	}
}

// parseSQLResult is the JSON shape returned by the `parse_sql` tool.
type parseSQLResult struct {
	ParserVersion string                         `json:"parser_version"`
	Statements    []sqlparse.ClassifiedStatement `json:"statements"`
}

// detectRiskyQueryHandler returns the handler for the
// `detect_risky_query` tool. It delegates to risk.Analyze for the
// actual analysis, so the MCP and CLI layers share one implementation.
func detectRiskyQueryHandler(parserVersion string) server.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		raw, exists := req.GetArguments()["sql"]
		if !exists {
			return mcp.NewToolResultError("sql parameter is required"), nil
		}
		sql, ok := raw.(string)
		if !ok {
			return mcp.NewToolResultError(fmt.Sprintf("sql parameter must be a string, got %T", raw)), nil
		}
		if sql == "" {
			return mcp.NewToolResultError("sql parameter must not be empty"), nil
		}

		findings, err := risk.Analyze(sql)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("parse error: %v", err)), nil
		}

		result := detectRiskyQueryResult{
			ParserVersion: parserVersion,
			Findings:      findings,
		}
		body, err := json.Marshal(result)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
		}
		return mcp.NewToolResultText(string(body)), nil
	}
}

// detectRiskyQueryResult is the JSON shape returned by the
// `detect_risky_query` tool.
type detectRiskyQueryResult struct {
	ParserVersion string         `json:"parser_version"`
	Findings      []risk.Finding `json:"findings"`
}
