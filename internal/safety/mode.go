// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package safety implements the Tier 3 statement allowlist that gates
// every cluster-bound command in crdb-sql (today: explain,
// explain-ddl). Defense-in-depth at the MCP/CLI layer: even if the
// downstream cluster's role would permit a write, a SELECT-only Mode
// rejects the statement before any pgwire round-trip.
//
// The package is split into three concerns:
//
//	mode.go      — the Mode enum (read_only, safe_write, full_access)
//	               and ParseMode for flag/parameter validation.
//	allowlist.go — Check, the pure AST classifier that decides whether
//	               a statement is permitted under a given Mode and
//	               Operation.
//	envelope.go  — Envelope, the helper that converts a Violation into
//	               the structured output.Error agents consume.
//
// allowlist.go has no dependency on internal/output, so the
// classification logic stays unit-testable without dragging in the CLI
// envelope. envelope.go is the single bridge between the two.
//
// Design doc reference: §Safety Model (read_only is the default,
// safe_write and full_access are opt-in escalations). Issue #21
// wired read_only end-to-end; issue #29 wired safe_write and
// full_access for OpExecute; issue #152 wired them for OpExplainDDL.
// OpExplain and OpSimulate still report "not yet implemented" for
// safe_write/full_access — wiring them is follow-up work.
package safety

import "fmt"

// Mode names the safety policy applied to a Tier 3 command. Values are
// the lowercase strings agents pass on the wire so the same token works
// across the CLI --mode flag and the MCP tool parameter.
type Mode string

// Mode values. ModeReadOnly is the default for every Tier 3 command;
// the other two are recognised by ParseMode and admitted by Check
// for OpExecute (issue #29) and OpExplainDDL (issue #152). For
// OpExplain and OpSimulate, safe_write and full_access still report
// "not yet implemented".
const (
	ModeReadOnly   Mode = "read_only"
	ModeSafeWrite  Mode = "safe_write"
	ModeFullAccess Mode = "full_access"
)

// DefaultMode is the safety mode applied when a caller does not set
// one. Every Tier 3 surface (CLI flag, MCP parameter) defaults to
// ModeReadOnly — explicit opt-in is required for any path that could
// reach a write.
const DefaultMode = ModeReadOnly

// ParseMode validates a user-supplied mode token and returns the
// canonical Mode. The empty string maps to DefaultMode so callers can
// pass a flag value through unconditionally without needing to
// special-case "unset". Any other unrecognised value produces an error
// that names the valid choices, so the message a user sees on a typo
// is actionable on its own.
//
// safe_write and full_access parse successfully for every Op even
// though Check rejects them for OpExplain and OpSimulate today —
// the split keeps the flag-parsing layer stable so the only churn
// when those modes land for the other surfaces is inside Check.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "":
		return DefaultMode, nil
	case ModeReadOnly:
		return ModeReadOnly, nil
	case ModeSafeWrite:
		return ModeSafeWrite, nil
	case ModeFullAccess:
		return ModeFullAccess, nil
	default:
		return "", fmt.Errorf("invalid safety mode %q: valid choices are %q, %q, %q",
			s, ModeReadOnly, ModeSafeWrite, ModeFullAccess)
	}
}
