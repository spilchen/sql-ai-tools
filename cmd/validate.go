// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/spf13/cobra"

	"github.com/spilchen/sql-ai-tools/internal/catalog"
	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/semcheck"
	"github.com/spilchen/sql-ai-tools/internal/sqlinput"
)

// validateChecks records which validation phases ran on the success
// path. Each field is "ok" (ran and passed) or "skipped" (prerequisite
// missing). A failing phase aborts the command via an envelope error,
// so "fail" never appears here.
type validateChecks struct {
	Syntax         string `json:"syntax"`
	TypeCheck      string `json:"type_check"`
	NameResolution string `json:"name_resolution"`
}

// validateResult is the JSON payload emitted by `crdb-sql validate` on
// the success path. The expanded shape (vs. a bare {valid: true})
// exposes which phases ran, so agents can tell whether name resolution
// was skipped due to a missing --schema. Adding a phase means adding a
// field here and updating the rendering code to set it.
type validateResult struct {
	Valid  bool           `json:"valid"`
	Checks validateChecks `json:"checks"`
}

const (
	checkOK      = "ok"
	checkSkipped = "skipped"

	// capabilityRequiredCode is the envelope error code emitted when a
	// validation phase is skipped because its prerequisite (e.g.
	// --schema) is not satisfied. The matching category is the same
	// string so agents can branch on either field.
	capabilityRequiredCode     = "capability_required"
	capabilityRequiredCategory = "capability_required"

	// capabilityNameResolution is the canonical identifier for the
	// table-name-resolution phase. It is shared between the phase's
	// skipped-warning Context and any human-readable message text so
	// the two cannot drift.
	capabilityNameResolution = "name_resolution"
)

// newValidateCmd builds the `crdb-sql validate` subcommand. It reads
// SQL from the -e flag, a positional file argument, or stdin, parses
// it with the CockroachDB parser, and reports whether the SQL is
// syntactically valid. On parse failure the error is surfaced as a
// structured envelope entry with SQLSTATE code, severity, message,
// and source position.
//
// When one or more --schema files are supplied the command additionally
// resolves table references against the loaded catalog and reports
// unknown tables (Tier 2). Without --schema, name resolution is skipped
// and the envelope carries a `capability_required` warning entry so
// agents can detect the missing capability rather than silently trust
// a partial result.
func newValidateCmd(state *rootState) *cobra.Command {
	var (
		expr        string
		schemaFiles []string
	)

	cmd := &cobra.Command{
		Use:   "validate [file]",
		Short: "Check SQL for syntax, type, and (with --schema) name errors",
		Long: `Parse SQL and report whether it is syntactically valid. On failure,
the error includes the SQLSTATE code, severity, message, and source
position (line/column/byte offset). Input is read from the -e flag
(inline SQL), a positional file argument, or stdin.

When --schema FILE is supplied (repeatable), table references in
SELECT/INSERT/UPDATE/DELETE statements are checked against the loaded
catalog and unknown tables are reported as 42P01 errors with an
"available_tables" context list. Without --schema, name resolution is
skipped and a capability_required warning is added to the envelope so
agents can tell that the check did not run.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tier := output.TierZeroConfig
			if len(schemaFiles) > 0 {
				tier = output.TierSchemaFile
			}
			r, baseEnv, err := newEnvelope(state, tier, cmd)
			if err != nil {
				return r.RenderError(baseEnv, err)
			}

			sql, err := sqlinput.ReadSQL(expr, args, cmd.InOrStdin())
			if err != nil {
				return r.RenderError(baseEnv, err)
			}

			// Trim surrounding whitespace so trailing newlines from
			// stdin/file do not skew position reporting.
			sql = strings.TrimSpace(sql)

			stmts, parseErr := parser.Parse(sql)
			if parseErr != nil {
				return renderParseError(r, baseEnv, parseErr, sql)
			}

			if typeErrs := semcheck.CheckExprTypes(stmts, sql); len(typeErrs) > 0 {
				return renderDiagErrors(r, baseEnv, typeErrs)
			}

			checks := validateChecks{
				Syntax:    checkOK,
				TypeCheck: checkOK,
			}

			if len(schemaFiles) == 0 {
				checks.NameResolution = checkSkipped
				baseEnv.Errors = append(baseEnv.Errors, capabilityRequiredError(
					capabilityNameResolution,
					"name resolution skipped: --schema not provided",
					"pass --schema FILE to enable table name resolution",
				))
			} else {
				cat, err := catalog.LoadFiles(schemaFiles)
				if err != nil {
					return renderSchemaLoadError(r, baseEnv, err)
				}
				appendSchemaWarnings(&baseEnv, cat)
				if nameErrs := semcheck.CheckTableNames(stmts, sql, cat); len(nameErrs) > 0 {
					return renderDiagErrors(r, baseEnv, nameErrs)
				}
				checks.NameResolution = checkOK
			}

			data, err := json.Marshal(validateResult{Valid: true, Checks: checks})
			if err != nil {
				return r.RenderError(baseEnv, err)
			}
			baseEnv.Data = data

			return r.Render(baseEnv, func(w io.Writer) error {
				if _, werr := fmt.Fprintln(w, "Valid."); werr != nil {
					return werr
				}
				if checks.NameResolution == checkSkipped {
					_, werr := fmt.Fprintln(w, "note: name resolution skipped (pass --schema to enable)")
					return werr
				}
				return nil
			})
		},
	}

	cmd.Flags().StringVarP(&expr, "expression", "e", "", "inline SQL to validate")
	cmd.Flags().StringArrayVar(&schemaFiles, "schema", nil,
		"schema SQL file(s) to enable table name resolution (repeatable)")

	return cmd
}

// capabilityRequiredError builds the warning entry that signals a
// validation phase was skipped because its prerequisite is missing.
// capability is the short identifier of the skipped phase (e.g.
// "name_resolution"); message is the user-facing summary; hint tells
// the user how to enable the phase. The result is appended to the
// envelope's Errors list rather than aborting the command — exit code
// stays 0 because the phases that did run all passed.
func capabilityRequiredError(capability, message, hint string) output.Error {
	return output.Error{
		Code:     capabilityRequiredCode,
		Severity: output.SeverityWarning,
		Message:  message,
		Category: capabilityRequiredCategory,
		Context: map[string]any{
			"capability": capability,
			"hint":       hint,
		},
	}
}

// renderParseError enriches a parser error with SQLSTATE code and
// position, then renders it through the standard envelope path. It
// bypasses Renderer.RenderError (which uses a generic
// "internal_error" code) so that agents receive the real SQLSTATE.
func renderParseError(r output.Renderer, env output.Envelope, parseErr error, sql string) error {
	return renderDiagErrors(r, env, []output.Error{diag.FromParseError(parseErr, sql)})
}

// renderDiagErrors renders one or more diagnostic errors (parse, type-
// check, or name resolution) through the standard envelope path. Any
// errors already attached to env (e.g. schema-load warnings) are
// preserved; the new errors are appended.
func renderDiagErrors(r output.Renderer, env output.Envelope, errs []output.Error) error {
	env.Errors = append(env.Errors, errs...)
	env.Data = nil

	if err := r.Render(env, func(w io.Writer) error {
		for _, diagErr := range errs {
			pos := diagErr.Position
			if pos != nil {
				if _, werr := fmt.Fprintf(w, "%d:%d: %s [%s]\n", pos.Line, pos.Column, diagErr.Message, diagErr.Code); werr != nil {
					return werr
				}
			} else {
				if _, werr := fmt.Fprintf(w, "%s [%s]\n", diagErr.Message, diagErr.Code); werr != nil {
					return werr
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return output.ErrRendered
}
