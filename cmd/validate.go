// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/spf13/cobra"

	"github.com/spilchen/sql-ai-tools/internal/catalog"
	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/semcheck"
	"github.com/spilchen/sql-ai-tools/internal/sqlinput"
	"github.com/spilchen/sql-ai-tools/internal/validateresult"
	"github.com/spilchen/sql-ai-tools/internal/version"
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
//
// When neither -e nor a file argument is given AND a crdb-sql.yaml
// config has been auto-discovered (or pointed at by --config), validate
// falls back to the project-wide path: every query file matched by the
// config is parsed, type-checked, and (per pair) name-resolved against
// the pair's schema globs, all reported in one envelope.
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

When --schema FILE is supplied (repeatable), table and column
references in SELECT/INSERT/UPDATE/DELETE statements are checked
against the loaded catalog. Unknown tables are reported as 42P01
errors with an "available_tables" context list; unknown columns as
42703 errors with "available_columns"; ambiguous unqualified refs
as 42702. Without --schema, name resolution is skipped and a
capability_required warning is added to the envelope so agents can
tell that the check did not run.

If no -e and no file argument are given and a crdb-sql.yaml config is
present in the working directory, validate iterates every query file
matched by the config and reports per-file results in one envelope.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if expr == "" && len(args) == 0 && len(schemaFiles) == 0 && state.cfg != nil {
				return runValidateConfig(state, cmd)
			}

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
				// A parse failure prevents every later phase from
				// running, so we report Syntax as the failed phase
				// and the rest as Skipped. Consumers can branch on
				// the Checks payload without inspecting error codes.
				parseChecks := validateresult.Checks{
					Syntax:         validateresult.CheckFailed,
					TypeCheck:      validateresult.CheckSkipped,
					NameResolution: validateresult.CheckSkipped,
				}
				return renderValidateFailure(r, baseEnv,
					[]output.Error{diag.FromParseError(parseErr, sql)}, parseChecks)
			}

			// Version-aware feature warnings are advisory: they
			// describe target-version compatibility independently
			// of whether semantic checks pass. Emit them
			// unconditionally so a user fixing a type error still
			// sees the upcoming-feature warnings in the same run.
			baseEnv.Errors = append(baseEnv.Errors,
				version.Inspect(stmts, state.targetVersion, nil)...)

			checks := validateresult.Checks{Syntax: validateresult.CheckOK}

			var cat *catalog.Catalog
			if len(schemaFiles) == 0 {
				baseEnv.Errors = append(baseEnv.Errors, validateresult.CapabilityRequiredError(
					validateresult.CapabilityNameResolution,
					"name resolution skipped: --schema not provided",
					"pass --schema FILE to enable table name resolution",
				))
			} else {
				var err error
				cat, err = catalog.LoadFiles(schemaFiles)
				if err != nil {
					return renderSchemaLoadError(r, baseEnv, err)
				}
				appendSchemaWarnings(&baseEnv, cat)
			}

			semRes, semErrs := semcheck.Run(stmts, sql, cat)
			checks.TypeCheck = semRes.TypeCheck
			checks.NameResolution = semRes.NameResolution

			if len(semErrs) > 0 {
				return renderValidateFailure(r, baseEnv, semErrs, checks)
			}

			data, err := json.Marshal(validateresult.Result{Valid: true, Checks: checks})
			if err != nil {
				return r.RenderError(baseEnv, err)
			}
			baseEnv.Data = data

			return r.Render(baseEnv, func(w io.Writer) error {
				if _, werr := fmt.Fprintln(w, "Valid."); werr != nil {
					return werr
				}
				if checks.NameResolution == validateresult.CheckSkipped {
					_, werr := fmt.Fprintln(w, "note: name resolution skipped (pass --schema to enable)")
					return werr
				}
				return nil
			})
		},
	}

	cmd.Flags().StringVarP(&expr, "expression", "e", "", "inline SQL to validate")
	cmd.Flags().StringArrayVar(&schemaFiles, "schema", nil,
		"schema SQL file(s) to enable table and column name resolution (repeatable)")

	return cmd
}

// fileResult is one entry in the JSON payload for the config-driven
// validate path. Each query file gets exactly one entry. ErrorCount
// counts ERROR-severity diagnostics only; advisory WARNING entries
// (today: version-aware feature warnings) are excluded so the field
// stays consistent with Valid — a file with valid=true cannot have
// error_count>0.
type fileResult struct {
	File       string `json:"file"`
	Valid      bool   `json:"valid"`
	ErrorCount int    `json:"error_count"`
}

// runValidateConfig executes the YAML-config fallback path of
// validate. It expands every SQLPair's globs, loads the schema set
// into a catalog (DDL parse failures abort the run), then parses and
// name-resolves each query file individually against that catalog.
// Per-file diagnostics accumulate into one envelope with a JSON
// payload describing each file's pass/fail status.
//
// Each SQLPair gets its own catalog so projects can keep production
// and test schemas separate. A pair with no schema globs runs syntax
// and type checks only — its query files are still validated, just
// without name resolution.
func runValidateConfig(state *rootState, cmd *cobra.Command) error {
	r, baseEnv, err := newEnvelope(state, output.TierSchemaFile, cmd)
	if err != nil {
		return r.RenderError(baseEnv, err)
	}

	cfg := state.cfg
	var (
		results       []fileResult
		queryAnyError bool
	)

	for _, pair := range cfg.SQL {
		schemaPaths, err := pair.ExpandSchema(cfg.BaseDir)
		if err != nil {
			return r.RenderError(baseEnv, err)
		}
		queryPaths, err := pair.ExpandQueries(cfg.BaseDir)
		if err != nil {
			return r.RenderError(baseEnv, err)
		}

		// Load the pair's schema set into a catalog. A DDL parse
		// failure is a config-level error: the project's source-of-
		// truth schema is broken, and no per-query result would be
		// trustworthy.
		var cat *catalog.Catalog
		if len(schemaPaths) > 0 {
			cat, err = catalog.LoadFiles(schemaPaths)
			if err != nil {
				return renderSchemaLoadError(r, baseEnv, err)
			}
			appendSchemaWarnings(&baseEnv, cat)
		}

		for _, qp := range queryPaths {
			fileErrs, ok := validateQueryFile(qp, cat, state.targetVersion)
			results = append(results, fileResult{
				File:       qp,
				Valid:      ok,
				ErrorCount: countErrors(fileErrs),
			})
			baseEnv.Errors = append(baseEnv.Errors, fileErrs...)
			if !ok {
				queryAnyError = true
			}
		}
	}

	data, err := json.Marshal(struct {
		Files []fileResult `json:"files"`
	}{Files: results})
	if err != nil {
		return r.RenderError(baseEnv, err)
	}
	baseEnv.Data = data

	renderErr := r.Render(baseEnv, func(w io.Writer) error {
		return renderConfigText(w, results, baseEnv.Errors)
	})
	if renderErr != nil {
		return renderErr
	}
	// Mirror the single-input path: a failed validation exits
	// non-zero via ErrRendered so CI pipelines (and the shell)
	// notice. queryAnyError covers per-file query problems; schema
	// warnings alone are not failures.
	if queryAnyError {
		return output.ErrRendered
	}
	return nil
}

// validateQueryFile reads one query file and runs the same parse +
// type-check pipeline used in the single-input path. When cat is
// non-nil, table-name resolution also runs against it. When
// targetVersion is non-empty, version-aware feature warnings are
// appended too. Each returned output.Error is tagged with the file
// path in Context["file"] so agents can attribute diagnostics across
// many files in one envelope.
//
// ok is true when no ERROR-severity diagnostics were found. Advisory
// WARNING-severity entries (today: version-aware feature warnings)
// are surfaced in errs but do not flip ok — the per-file Valid
// status mirrors the single-input path, where warnings are advisory.
func validateQueryFile(
	path string, cat *catalog.Catalog, targetVersion string,
) (errs []output.Error, ok bool) {
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		return []output.Error{{
			Code:     "io_error",
			Severity: output.SeverityError,
			Message:  fmt.Sprintf("read query file: %v", readErr),
			Context:  map[string]any{"file": path},
		}}, false
	}
	sql := strings.TrimSpace(string(data))
	if sql == "" {
		// An empty query file is not an error: glob matches can pick
		// up partial files in development. Reporting "file is empty"
		// would be noise for the common case of `git checkout` mid-
		// edit.
		return nil, true
	}

	stmts, parseErr := parser.Parse(sql)
	if parseErr != nil {
		e := diag.FromParseError(parseErr, sql)
		return []output.Error{tagFile(e, path)}, false
	}

	// semcheck.Run returns only hard ERROR-severity diagnostics
	// (type-check, table-name, column-name); the runner already
	// accumulates across phases without early returns. Version
	// warnings are appended afterwards as advisories — they do not
	// flip the per-file Valid flag, mirroring the single-input
	// path's treatment of warnings.
	_, semErrs := semcheck.Run(stmts, sql, cat)
	versionWarns := version.Inspect(stmts, targetVersion, nil)
	if len(semErrs) == 0 && len(versionWarns) == 0 {
		return nil, true
	}
	fileErrs := make([]output.Error, 0, len(semErrs)+len(versionWarns))
	for _, e := range semErrs {
		fileErrs = append(fileErrs, tagFile(e, path))
	}
	for _, e := range versionWarns {
		fileErrs = append(fileErrs, tagFile(e, path))
	}
	return fileErrs, len(semErrs) == 0
}

// countErrors returns the number of ERROR-severity entries in errs,
// excluding advisory warnings. Used to keep fileResult.ErrorCount
// consistent with fileResult.Valid: an advisory-only file should
// report error_count=0 alongside valid=true.
func countErrors(errs []output.Error) int {
	n := 0
	for _, e := range errs {
		if e.Severity == output.SeverityError {
			n++
		}
	}
	return n
}

// tagFile attaches the source file path to an error's Context so
// downstream consumers (agents, CI runners) can group diagnostics by
// file. Existing Context entries are preserved.
//
// The returned Error always owns a fresh Context map: output.Error is
// passed by value but its Context field is a map header, so without a
// copy the caller's map would be mutated and the "file" tag could
// bleed across files if upstream code reuses error templates.
func tagFile(e output.Error, path string) output.Error {
	if e.Context == nil {
		e.Context = map[string]any{"file": path}
		return e
	}
	ctx := make(map[string]any, len(e.Context)+1)
	for k, v := range e.Context {
		ctx[k] = v
	}
	ctx["file"] = path
	e.Context = ctx
	return e
}

// renderConfigText is the human-readable layout for the config-driven
// validate path. One line per file: either "PATH: valid" or, for
// failed files, the per-file diagnostics with line/column/code.
// Schema-level warnings (no associated file) print first.
func renderConfigText(w io.Writer, results []fileResult, errs []output.Error) error {
	for _, e := range errs {
		if _, hasFile := e.Context["file"]; hasFile {
			continue
		}
		if _, err := fmt.Fprintf(w, "[%s] %s\n", e.Severity, e.Message); err != nil {
			return err
		}
	}

	errsByFile := make(map[string][]output.Error, len(results))
	for _, e := range errs {
		path, ok := e.Context["file"].(string)
		if !ok {
			continue
		}
		errsByFile[path] = append(errsByFile[path], e)
	}

	for _, fr := range results {
		if fr.Valid {
			if _, err := fmt.Fprintf(w, "%s: valid\n", fr.File); err != nil {
				return err
			}
			continue
		}
		for _, e := range errsByFile[fr.File] {
			pos := e.Position
			if pos != nil {
				if _, err := fmt.Fprintf(w, "%s:%d:%d: %s [%s]\n",
					fr.File, pos.Line, pos.Column, e.Message, e.Code); err != nil {
					return err
				}
			} else if _, err := fmt.Fprintf(w, "%s: %s [%s]\n", fr.File, e.Message, e.Code); err != nil {
				return err
			}
			if err := writeSuggestions(w, e.Suggestions); err != nil {
				return err
			}
		}
	}
	return nil
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
		return writeDiagErrors(w, errs)
	}); err != nil {
		return err
	}
	return output.ErrRendered
}

// renderValidateFailure renders a validate run that produced one or
// more errors. checks records the per-phase outcome and is marshalled
// into env.Data as a Result{Valid:false} payload so JSON consumers
// always learn which phases ran (and which were skipped because an
// upstream phase failed). errs is appended to env.Errors and printed
// in text mode through the writeDiagErrors helper shared with
// renderDiagErrors.
func renderValidateFailure(
	r output.Renderer, env output.Envelope, errs []output.Error, checks validateresult.Checks,
) error {
	// Append diagnostics to the envelope before marshaling so that
	// even an (essentially impossible) Marshal failure of the tiny
	// Result struct still surfaces every error the caller wanted to
	// report. RenderError preserves env.Errors when promoting to the
	// generic internal_error path.
	env.Errors = append(env.Errors, errs...)

	data, err := json.Marshal(validateresult.Result{Valid: false, Checks: checks})
	if err != nil {
		return r.RenderError(env, err)
	}
	env.Data = data

	if err := r.Render(env, func(w io.Writer) error {
		return writeDiagErrors(w, errs)
	}); err != nil {
		return err
	}
	return output.ErrRendered
}

// writeDiagErrors prints one line per diagnostic error in text mode,
// followed by any "did you mean" suggestion lines. Shared by the two
// validate failure-rendering paths so a future formatting tweak (e.g.
// adding the SQLSTATE class name) only needs to change one place.
func writeDiagErrors(w io.Writer, errs []output.Error) error {
	for _, diagErr := range errs {
		pos := diagErr.Position
		if pos != nil {
			if _, werr := fmt.Fprintf(w, "%d:%d: %s [%s]\n", pos.Line, pos.Column, diagErr.Message, diagErr.Code); werr != nil {
				return werr
			}
		} else if _, werr := fmt.Fprintf(w, "%s [%s]\n", diagErr.Message, diagErr.Code); werr != nil {
			return werr
		}
		if werr := writeSuggestions(w, diagErr.Suggestions); werr != nil {
			return werr
		}
	}
	return nil
}

// writeSuggestions prints structured "did you mean?" suggestions
// underneath an error in text mode. Each suggestion gets one indented
// line with the replacement and a percentage confidence — terse
// enough to stay readable when many errors stack up, structured
// enough that grep/awk can pick the lines out by the leading "  did
// you mean: " prefix.
func writeSuggestions(w io.Writer, suggs []output.Suggestion) error {
	for _, s := range suggs {
		if _, err := fmt.Fprintf(w, "  did you mean: %s (%d%% confidence)\n",
			s.Replacement, int(s.Confidence*100+0.5)); err != nil {
			return err
		}
	}
	return nil
}
