// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package mcp builds the crdb-sql Model Context Protocol server.
//
// The server registers a health-check tool (ping), five Tier 1 SQL
// tools (parse_sql, validate_sql, format_sql, detect_risky_sql,
// summarize_sql), two Tier 2 catalog tools (list_tables,
// describe_table) that operate on inline CREATE TABLE schemas, and
// four Tier 3 connected tools (explain_sql, explain_schema_change,
// simulate_sql, execute_sql) that run against a live cluster.
// validate_sql is dual-tier: it runs Tier 1 by default and lifts to
// Tier 2 (name resolution) when the caller supplies inline schemas.
// Keeping construction pure (no transport, no I/O) lets the cmd layer
// pick a transport — currently just stdio — and lets tests exercise
// individual tool handlers directly.
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

	"github.com/spilchen/sql-ai-tools/internal/mcp/proxy"
	"github.com/spilchen/sql-ai-tools/internal/mcp/tools"
	"github.com/spilchen/sql-ai-tools/internal/versionroute"
)

// PingToolName is the registered MCP tool name for the health-check tool.
// All other tool name constants live in the tools subpackage.
const PingToolName = "ping"

// Option configures NewServer. The pattern matches CockroachDB's
// functional-options convention: each Option mutates a
// serverOptions holder so the public NewServer signature stays
// stable going forward as new knobs arrive. Today the configurable
// knobs are the per-call target_version Router (issue #129) and
// the built-quarter override used by tests; the same shape extends
// naturally to future server-wide options.
type Option interface {
	apply(*serverOptions)
}

// serverOptions is the internal config aggregator the Option
// constructors mutate.
//
//   - router is the per-call target_version Router used to forward
//     tool calls whose quarter does not match the running binary.
//   - builtOverride, when non-zero, replaces versionroute.Built()
//     for the purpose of routing decisions. Production callers
//     leave this unset; tests set it to drive the wrapper into a
//     known routing decision without having to fake the binary
//     filename or set builtQuarterStamp at link time.
type serverOptions struct {
	router        proxy.Router
	builtOverride versionroute.Quarter
}

type optionFunc func(*serverOptions)

func (f optionFunc) apply(o *serverOptions) { f(o) }

// WithRouter installs the per-call target_version Router used by
// every parser-dependent tool handler. When omitted, NewServer wires
// proxy.NoopRouter, which returns a clear "routing not enabled" tool
// error rather than silently answering with the wrong parser. The
// production binary (cmd/mcp.go) installs proxy.NewSpawnRouter().
func WithRouter(r proxy.Router) Option {
	return optionFunc(func(o *serverOptions) { o.router = r })
}

// WithBuiltQuarter overrides the built quarter used for routing
// decisions. Production callers must not use this — versionroute.Built()
// is the source of truth for which sibling backend "this binary"
// is. Tests use it to construct a server with a known built
// quarter without having to fiddle with link-time stamps or fake
// the executable path.
func WithBuiltQuarter(q versionroute.Quarter) Option {
	return optionFunc(func(o *serverOptions) { o.builtOverride = q })
}

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
// The variadic Option values configure cross-cutting behavior — most
// notably the per-call target_version Router (see WithRouter). The
// returned server has no transport bound; callers wire it to stdio
// (or, in the future, sse/http) themselves.
//
// Per-call routing wiring: the nine parser-dependent tool handlers
// (parse_sql, validate_sql, format_sql, detect_risky_sql,
// summarize_sql, explain_sql, explain_schema_change, simulate_sql,
// execute_sql) are wrapped with withRouting so a target_version
// whose quarter differs from the running binary forwards to a
// sibling backend. The three tools that don't take target_version
// (ping, list_tables, describe_table) are registered unwrapped.
func NewServer(
	crdbSQLVersion, parserVersion, defaultTargetVersion string, opts ...Option,
) *server.MCPServer {
	cfg := serverOptions{router: proxy.NoopRouter{}}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	// Built() returning (zero, false) — unstamped binary with no
	// parser dep recorded in BuildInfo — is treated by withRouting
	// as "no routing"; an operator hitting that case sees
	// versionroute.StampDiagnostic on stderr at process startup
	// (emitted by versionroute.MaybeReexec), so silent failure here
	// is bounded.
	built, _ := versionroute.Built()
	if !cfg.builtOverride.IsZero() {
		built = cfg.builtOverride
	}

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
	// route wraps a parser-dependent handler with the per-call
	// target_version router. Defined as a closure so the per-tool
	// AddTool calls below stay one-liners.
	route := func(h server.ToolHandlerFunc) server.ToolHandlerFunc {
		return withRouting(h, defaultTargetVersion, built, cfg.router)
	}
	s.AddTool(tools.ParseSQLTool(), route(tools.ParseSQLHandler(parserVersion, defaultTargetVersion)))
	s.AddTool(tools.ValidateSQLTool(), route(tools.ValidateSQLHandler(parserVersion, defaultTargetVersion)))
	s.AddTool(tools.FormatSQLTool(), route(tools.FormatSQLHandler(parserVersion, defaultTargetVersion)))
	s.AddTool(tools.DetectRiskySQLTool(), route(tools.DetectRiskySQLHandler(parserVersion, defaultTargetVersion)))
	s.AddTool(tools.SummarizeSQLTool(), route(tools.SummarizeSQLHandler(parserVersion, defaultTargetVersion)))
	s.AddTool(tools.ExplainSQLTool(), route(tools.ExplainSQLHandler(parserVersion, defaultTargetVersion)))
	s.AddTool(tools.ExplainSchemaChangeTool(), route(tools.ExplainSchemaChangeHandler(parserVersion, defaultTargetVersion)))
	s.AddTool(tools.SimulateSQLTool(), route(tools.SimulateSQLHandler(parserVersion, defaultTargetVersion)))
	s.AddTool(tools.ExecuteSQLTool(), route(tools.ExecuteSQLHandler(parserVersion, defaultTargetVersion)))
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
