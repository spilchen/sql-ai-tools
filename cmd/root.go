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
	"fmt"
	"io"
	"os"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/pgwire/pgerror"
	"github.com/spf13/cobra"

	"github.com/spilchen/sql-ai-tools/internal/catalog"
	"github.com/spilchen/sql-ai-tools/internal/config"
	"github.com/spilchen/sql-ai-tools/internal/conn"
	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/schemawarn"
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

	// cfg is the parsed crdb-sql.yaml, or nil when no config file was
	// found in CWD (or pointed at by --config). Subcommands treat nil
	// as "no project config; use flags" — config consumption is
	// opt-in per subcommand. Populated by PersistentPreRunE.
	cfg *config.File

	// targetVersion is the canonical CockroachDB version the user
	// declared via --target-version. The empty string is the
	// sentinel for "flag not supplied"; newEnvelope (and the MCP
	// server, which forwards it as the per-call default) reads this
	// to decide whether to stamp the field and emit a mismatch
	// warning. "Canonical" means the leading "v" (if any) has been
	// stripped and components have been validated as unsigned
	// integers; ValidateTargetVersion is the sole producer.
	// Populated by PersistentPreRunE after format validation.
	targetVersion string
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
round-tripping through a live cluster.

Secure clusters are supported transparently: TLS parameters can ride
inside --dsn as libpq URI params (sslmode, sslrootcert, sslcert, sslkey)
or be supplied via the matching --ssl* flags. See the README "Connecting
to a secure cluster" section for examples.`,
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

			tls, err := readTLSFlags(cmd)
			if err != nil {
				return err
			}
			if !tls.IsZero() {
				// MergeTLSParams enforces the form policy (URI required)
				// and the conflict policy (no silent overrides). Both
				// are returned as plain errors so the caller surfaces
				// them via the standard exit-1 path used for missing or
				// malformed --dsn input.
				merged, err := conn.MergeTLSParams(state.dsn, tls)
				if err != nil {
					return err
				}
				state.dsn = merged
			}

			cfgPath, err := cmd.Flags().GetString(configFlag)
			if err != nil {
				return err
			}
			cfg, err := loadConfig(cfgPath)
			if err != nil {
				return err
			}
			state.cfg = cfg

			rawTarget, err := cmd.Flags().GetString(targetVersionFlag)
			if err != nil {
				return err
			}
			if rawTarget != "" {
				canonical, err := output.ValidateTargetVersion(rawTarget)
				if err != nil {
					return fmt.Errorf("--%s: %w", targetVersionFlag, err)
				}
				state.targetVersion = canonical
			}
			return nil
		},
	}
	root.PersistentFlags().StringP(outputFlag, "o", string(output.FormatText),
		`output format: "text" or "json"`)
	root.PersistentFlags().String(dsnFlag, "",
		"CockroachDB connection string (overrides CRDB_DSN env var); TLS params can ride inside as libpq URI params (sslmode/sslrootcert/sslcert/sslkey) or via the --ssl* flags below")
	root.PersistentFlags().String(configFlag, "",
		"path to crdb-sql.yaml (default: auto-discover in CWD)")
	root.PersistentFlags().String(targetVersionFlag, "",
		"Target CockroachDB version (e.g. 25.4.0); reported in the response envelope")
	root.PersistentFlags().String(sslModeFlag, "",
		`TLS verification mode (e.g. "verify-full", "require"); merged into --dsn as ?sslmode=`)
	root.PersistentFlags().String(sslRootCertFlag, "",
		"path to the trusted CA certificate; merged into --dsn as ?sslrootcert=")
	root.PersistentFlags().String(sslCertFlag, "",
		"path to the client certificate (cert-based auth); merged into --dsn as ?sslcert=")
	root.PersistentFlags().String(sslKeyFlag, "",
		"path to the client private key (cert-based auth); merged into --dsn as ?sslkey=")
	root.AddCommand(newVersionCmd(state))
	root.AddCommand(newVersionsCmd(state))
	root.AddCommand(newPingCmd(state))
	root.AddCommand(newParseCmd(state))
	root.AddCommand(newFormatCmd(state))
	root.AddCommand(newValidateCmd(state))
	root.AddCommand(newDescribeCmd(state))
	root.AddCommand(newListTablesCmd(state))
	root.AddCommand(newRiskCmd(state))
	root.AddCommand(newSummarizeCmd(state))
	root.AddCommand(newExplainCmd(state))
	root.AddCommand(newExplainDDLCmd(state))
	root.AddCommand(newSimulateCmd(state))
	root.AddCommand(newExecCmd(state))
	root.AddCommand(newMCPCmd(state))
	return root
}

// outputFlag is the name of the persistent --output flag. It is shared
// between the root command's flag registration and PersistentPreRunE
// lookup so the two stay in sync.
const (
	outputFlag        = "output"
	dsnFlag           = "dsn"
	configFlag        = "config"
	targetVersionFlag = "target-version"
	sslModeFlag       = "sslmode"
	sslRootCertFlag   = "sslrootcert"
	sslCertFlag       = "sslcert"
	sslKeyFlag        = "sslkey"
)

// readTLSFlags reads the four --ssl* persistent flags into a
// conn.TLSParams. Any GetString failure is propagated unchanged so the
// caller can surface it the same way it would surface an unknown
// --output value (cobra returns these only when the flag is not
// registered, which is a programming error).
func readTLSFlags(cmd *cobra.Command) (conn.TLSParams, error) {
	mode, err := cmd.Flags().GetString(sslModeFlag)
	if err != nil {
		return conn.TLSParams{}, err
	}
	rootCert, err := cmd.Flags().GetString(sslRootCertFlag)
	if err != nil {
		return conn.TLSParams{}, err
	}
	cert, err := cmd.Flags().GetString(sslCertFlag)
	if err != nil {
		return conn.TLSParams{}, err
	}
	key, err := cmd.Flags().GetString(sslKeyFlag)
	if err != nil {
		return conn.TLSParams{}, err
	}
	return conn.TLSParams{
		SSLMode:     mode,
		SSLRootCert: rootCert,
		SSLCert:     cert,
		SSLKey:      key,
	}, nil
}

// loadConfig resolves the project YAML config. When path is non-empty,
// the file at that path must exist and parse cleanly (explicit user
// intent — fail loudly). When path is empty, Discover looks in CWD;
// absence is silently tolerated so commands work outside configured
// projects.
func loadConfig(path string) (*config.File, error) {
	if path != "" {
		return config.Load(path)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("get working directory: %w", err)
	}
	return config.Discover(cwd)
}

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
	if state.targetVersion != "" {
		env.TargetVersion = state.targetVersion
		if warning, ok := output.VersionMismatchWarning(parserVer, state.targetVersion); ok {
			env.Errors = append(env.Errors, warning)
		}
	}
	return r, env, nil
}

// appendSchemaWarnings is a thin alias for schemawarn.Append kept on
// the cmd package so subcommand code stays terse.
func appendSchemaWarnings(env *output.Envelope, cat *catalog.Catalog) {
	schemawarn.Append(env, cat)
}

// renderSchemaLoadError surfaces a catalog.Load/LoadFiles failure as
// a structured envelope error rather than a generic "internal_error".
// When the underlying cause carries a SQLSTATE (typically a 42601
// parse error from a malformed schema file), that code is propagated
// so agents can branch on it; otherwise the error is tagged
// "schema_load_error" for I/O and validation failures.
//
// Returns output.ErrRendered so the caller signals failure to
// cmd/crdb-sql/main.go the same way renderDiagErrors does.
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
