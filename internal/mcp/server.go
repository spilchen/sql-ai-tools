// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package mcp builds the crdb-sql Model Context Protocol server.
//
// Today the server only registers the `ping` health-check tool; future
// issues will hang real SQL-aware tools (validate_sql, format_sql, …)
// off the same NewServer constructor. Keeping construction pure (no
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
)

// PingToolName is the registered name of the health-check tool. Exposed
// as a constant so tests and the README/docs reference a single source
// of truth.
const PingToolName = "ping"

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
