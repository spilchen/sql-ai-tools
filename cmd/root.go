// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package cmd hosts the cobra command tree for the crdb-sql CLI.
//
// The root is a thin shell: each subcommand (validate, format, parse,
// etc.) is defined in its own file and attached via newRootCmd, which
// builds a fresh tree per call. Avoiding package-global commands keeps
// tests independent (no flag-state leakage between t.Run cases) and
// removes the need for init()-time registration.
package cmd

import (
	"context"
	"io"
	"os"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/pgwire/pgerror"
	"github.com/spf13/cobra"

	"github.com/spilchen/sql-ai-tools/internal/catalog"
	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/output"
)

// rootState is the per-invocation container for cobra-resolved global
// state that subcommands need to read. It is populated by the root
// command's PersistentPreRunE (which runs after flag parsing, before
// any subcommand RunE) and read by subcommand RunE closures via the
// pointer captured at construction time.
//
// Lifecycle: one instance per newRootCmd call, discarded when Execute
// returns. This is what keeps tests independent — package globals would
// leak the previous test's --output value into the next.
type rootState struct {
	// outputFormat is the validated --output value. Subcommands read it
	// after PersistentPreRunE has run; reading it earlier yields the
	// zero value.
	outputFormat output.Format

	// dsn is the resolved CockroachDB connection string. Empty when
	// neither --dsn nor CRDB_DSN was provided; commands that require
	// a connection check for empty and return a structured error.
	// Populated by PersistentPreRunE.
	dsn string
}

// newRootCmd builds a fresh root command with all subcommands attached.
// Construct one per Execute call (and per test) so cobra's parsed-flag
// state never leaks across invocations.
func newRootCmd() *cobra.Command {
	state := &rootState{}
	root := &cobra.Command{
		Use:   "crdb-sql",
		Short: "Agent-friendly SQL tooling for CockroachDB",
		Long: `crdb-sql exposes CockroachDB's parser, type system, and structured
error infrastructure as a CLI and MCP server so that
AI agents can validate, format, and reason about CockroachDB SQL without
round-tripping through a live cluster.`,
		// Both silences are deliberate: cobra should neither print the
		// usage dump on a runtime error (noisy) nor print the error
		// itself (we want a single source of truth). The Execute caller
		// owns error printing and exit-code translation; do not flip
		// these without updating that caller.
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := cmd.Flags().GetString(outputFlag)
			if err != nil {
				return err
			}
			f, err := output.ParseFormat(raw)
			if err != nil {
				return err
			}
			state.outputFormat = f

			dsn, err := cmd.Flags().GetString(dsnFlag)
			if err != nil {
				return err
			}
			if dsn != "" {
				state.dsn = dsn
			} else {
				state.dsn = os.Getenv("CRDB_DSN")
			}
			return nil
		},
	}
	root.PersistentFlags().StringP(outputFlag, "o", string(output.FormatText),
		`output format: "text" or "json"`)
	root.PersistentFlags().String(dsnFlag, "",
		"CockroachDB connection string (overrides CRDB_DSN env var)")
	root.AddCommand(newVersionCmd(state))
	root.AddCommand(newPingCmd(state))
	root.AddCommand(newParseCmd(state))
	root.AddCommand(newFormatCmd(state))
	root.AddCommand(newValidateCmd(state))
	root.AddCommand(newDescribeCmd(state))
	root.AddCommand(newListTablesCmd(state))
	root.AddCommand(newRiskCmd(state))
	root.AddCommand(newExplainCmd(state))
	root.AddCommand(newMCPCmd())
	return root
}

// outputFlag is the name of the persistent --output flag. It is shared
// between the root command's flag registration and PersistentPreRunE
// lookup so the two stay in sync.
const (
	outputFlag = "output"
	dsnFlag    = "dsn"
)

// Execute runs the root command against process arguments and returns
// whatever cobra surfaces. It does not print the error or call
// os.Exit; the caller owns that translation. This keeps the cmd
// package importable from tests without side effects on process state.
func Execute() error {
	return newRootCmd().ExecuteContext(context.Background())
}

// newEnvelope builds the Renderer and base Envelope that each CLI
// subcommand needs. On success the returned Envelope has its Tier,
// ParserVersion, and ConnectionStatus populated; the caller owns Data
// and may append to Errors via RenderError. On error ParserVersion
// may be empty, but the Renderer and Envelope are still usable — the
// caller should typically do:
//
//	r, env, err := newEnvelope(state, output.TierZeroConfig, cmd)
//	if err != nil {
//	    return r.RenderError(env, err)
//	}
func newEnvelope(
	state *rootState, tier output.Tier, cmd *cobra.Command,
) (output.Renderer, output.Envelope, error) {
	r := output.Renderer{Format: state.outputFormat, Out: cmd.OutOrStdout()}
	env := output.Envelope{
		Tier:             tier,
		ConnectionStatus: output.ConnectionDisconnected,
	}
	parserVer, err := parserVersion(Version)
	if err != nil {
		return r, env, err
	}
	env.ParserVersion = parserVer
	return r, env, nil
}

// appendSchemaWarnings copies any non-fatal issues recorded by the
// catalog loader (skipped statements, duplicate definitions) into env
// as warning-severity envelope entries. Subcommands that consume a
// catalog call this once after Load/LoadFiles so agents see the
// loader's diagnostics in the same Errors stream as everything else.
func appendSchemaWarnings(env *output.Envelope, cat *catalog.Catalog) {
	for _, w := range cat.Warnings() {
		env.Errors = append(env.Errors, output.Error{
			Code:     "schema_warning",
			Severity: output.SeverityWarning,
			Message:  w,
		})
	}
}

// renderSchemaLoadError surfaces a catalog.Load/LoadFiles failure as
// a structured envelope error rather than a generic "internal_error".
// When the underlying cause carries a SQLSTATE (typically a 42601
// parse error from a malformed schema file), that code is propagated
// so agents can branch on it; otherwise the error is tagged
// "schema_load_error" for I/O and validation failures.
//
// Returns output.ErrRendered so the caller signals failure to main.go
// the same way renderDiagErrors does.
func renderSchemaLoadError(r output.Renderer, env output.Envelope, err error) error {
	// pgerror.GetPGCode returns "XXUUU" (Uncategorized) when the
	// error chain has no SQLSTATE attached — typical for I/O errors
	// from os.Stat / os.ReadFile. Treat that as "no real code" so we
	// fall through to the dedicated schema_load_error tag.
	code := pgerror.GetPGCode(err).String()
	if code == "" || code == "XXUUU" {
		code = "schema_load_error"
	}
	category := diag.CategoryForCode(code)
	env.Errors = append(env.Errors, output.Error{
		Code:     code,
		Severity: output.SeverityError,
		Message:  err.Error(),
		Category: category,
	})
	env.Data = nil
	if rerr := r.Render(env, func(w io.Writer) error {
		_, werr := io.WriteString(w, err.Error()+"\n")
		return werr
	}); rerr != nil {
		return rerr
	}
	return output.ErrRendered
}
