// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"errors"
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
$PATH. Routed calls go through a warm sibling-process pool — the
first call to a target version pays one process spawn plus the MCP
initialize handshake; subsequent calls reuse the warm child until it
has been idle for ~5 minutes. The pool drains cleanly when the MCP
client closes stdin (the normal exit path); SIGTERM/SIGKILL of the
parent skips the drain and orphans any warm sibling children.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) (retErr error) {
			// Resolve the parser version up front so a stamped release
			// build with a missing dep fails fast — same hard-fail
			// behavior the version subcommand uses, rather than letting
			// the server come up reporting a confusing empty string.
			parserVer, err := parserVersion(Version)
			if err != nil {
				return err
			}
			pool := proxy.NewPoolRouter()
			// Defer pool shutdown so warm sibling children get their
			// stdin closed (clean exit) when the MCP client closes
			// our stdin and ServeStdio unwinds. Without this every
			// pooled sibling would survive the parent. Join any
			// shutdown error onto the named return so cobra's exit
			// code reflects "siblings refused to drain" — silently
			// logging to stderr would let an orphan-process problem
			// escape with exit 0.
			defer func() {
				if cerr := pool.Close(); cerr != nil {
					retErr = errors.Join(retErr, fmt.Errorf("pool shutdown: %w", cerr))
				}
			}()
			s := internalmcp.NewServer(
				Version, parserVer, state.targetVersion,
				internalmcp.WithRouter(pool),
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
