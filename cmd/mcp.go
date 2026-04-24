// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"fmt"

	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"

	internalmcp "github.com/spilchen/sql-ai-tools/internal/mcp"
	"github.com/spilchen/sql-ai-tools/internal/mcp/proxy"
)

// newMCPCmd builds the `crdb-sql mcp` subcommand. It launches an MCP
// server bound to stdio so an MCP client (Claude Code, VS Code, etc.)
// can spawn the binary and discover the registered tools. The command
// blocks until the client disconnects (stdin closes); a clean
// disconnect returns nil, anything else surfaces as the cobra error.
// The registered tools are defined in internal/mcp.NewServer.
//
// state.targetVersion (if set via --target-version) is forwarded to
// the server as a default applied to every tool call; per-call
// target_version arguments override it.
func newMCPCmd(state *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Run the crdb-sql MCP server on stdio",
		Long: `Start an MCP (Model Context Protocol) server that speaks JSON-RPC
over stdio. Intended to be launched by an MCP client (e.g. Claude Code
via "claude mcp add"); the process exits when the client closes stdin.

Per-call target_version routing: any tool call whose target_version
maps to a different parser quarter than this binary's bundled parser
is forwarded to the matching sibling backend (crdb-sql-vXXX) on
$PATH. The first cut spawns one sibling subprocess per routed call;
expect ~tens-of-ms overhead per call vs. local handlers (benchmark
tracked in #146; warm pooling in #145).`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// Resolve the parser version up front so a stamped release
			// build with a missing dep fails fast — same hard-fail
			// behavior the version subcommand uses, rather than letting
			// the server come up reporting a confusing empty string.
			parserVer, err := parserVersion(Version)
			if err != nil {
				return err
			}
			s := internalmcp.NewServer(
				Version, parserVer, state.targetVersion,
				internalmcp.WithRouter(proxy.NewSpawnRouter()),
			)
			// Wrap the transport error so a failure in the stdio loop
			// surfaces through cobra's "Error:" line as obviously
			// transport-layer rather than as an opaque message from
			// somewhere deep in mcp-go. %w preserves errors.Is/As for
			// any caller that wants to distinguish specific causes.
			if err := server.ServeStdio(s); err != nil {
				return fmt.Errorf("mcp stdio server: %w", err)
			}
			return nil
		},
	}
}
