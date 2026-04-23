// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package validateresult defines the JSON payload shape and shared
// constants used by the SQL validation surfaces (the `crdb-sql validate`
// CLI command and the `validate_sql` MCP tool). Centralizing these
// symbols lets the two surfaces emit the same envelope so agents can
// rely on a single contract — adding a new validation phase means
// editing one place, not two.
//
// The package has a single inbound dependency (internal/output) and no
// transitive deps on cmd or MCP code, so either surface can import it
// without pulling in the other.
package validateresult

import "github.com/spilchen/sql-ai-tools/internal/output"

// CheckStatus is the per-phase outcome reported in Checks. Defined as
// a named string so call sites get compile-time checking against the
// closed set of legal values — bare strings would let a typo like
// "okay" reach the wire. JSON marshaling is unchanged.
type CheckStatus string

// CheckStatus values. CheckOK means the phase ran and produced no
// errors; CheckSkipped means its prerequisite (typically --schema or
// the schemas MCP argument) was missing; CheckFailed means the phase
// ran but produced one or more errors. The failure path emits Checks
// alongside Errors so consumers can attribute errors to phases and
// see which downstream phases (if any) were skipped because an
// upstream phase could not produce usable input.
const (
	CheckOK      CheckStatus = "ok"
	CheckSkipped CheckStatus = "skipped"
	CheckFailed  CheckStatus = "failed"
)

// CapabilityRequiredCode is the envelope error Code (and Category) for
// a skipped validation phase. Reusing one constant for both fields —
// rather than defining two separate constants with the same literal —
// makes drift impossible.
const CapabilityRequiredCode = "capability_required"

// CapabilityNameResolution is the canonical short identifier of the
// table-name-resolution phase, embedded both in the warning's Context
// and in the Checks field name so the two cannot drift.
const CapabilityNameResolution = "name_resolution"

// Checks records the per-phase outcome for a validation run. Each
// field is CheckOK, CheckSkipped, or CheckFailed. Both the success
// and failure paths populate this struct so agents always learn which
// phases ran. Adding a phase means adding a field here and updating
// both surfaces (CLI and MCP) on each path (success and failure) — four
// rendering sites total.
type Checks struct {
	Syntax         CheckStatus `json:"syntax"`
	TypeCheck      CheckStatus `json:"type_check"`
	NameResolution CheckStatus `json:"name_resolution"`
}

// Result is the JSON payload for SQL validation. Valid is true on
// the success path (no errors emitted) and false when one or more
// phases produced errors. Checks reports the per-phase outcome in
// either case so agents can tell whether name resolution was
// skipped, ran cleanly, or failed.
type Result struct {
	Valid  bool   `json:"valid"`
	Checks Checks `json:"checks"`
}

// CapabilityRequiredError builds the warning entry that signals a
// validation phase was skipped because its prerequisite is missing.
// capability is the short identifier of the skipped phase (e.g.
// CapabilityNameResolution); message is the user-facing summary; hint
// tells the user how to enable the phase. The result is appended to
// the envelope's Errors list rather than aborting the request — the
// CLI exit code stays 0 (or the MCP tool result stays successful)
// because the phases that did run all passed.
func CapabilityRequiredError(capability, message, hint string) output.Error {
	return output.Error{
		Code:     CapabilityRequiredCode,
		Severity: output.SeverityWarning,
		Message:  message,
		Category: CapabilityRequiredCode,
		Context: map[string]any{
			"capability": capability,
			"hint":       hint,
		},
	}
}
