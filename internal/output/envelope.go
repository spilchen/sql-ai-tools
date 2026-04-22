// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package output defines the shared response shape every crdb-sql
// subcommand emits when --output=json, plus the renderer that switches
// between text and JSON.
//
// The Envelope describes the analysis context — which tier produced the
// result, which parser version was used, whether a cluster was reached —
// and carries a uniform Errors list for agent consumption. Subcommand-
// specific payload lives in Data so the envelope itself never needs to
// learn about new commands. The schema is derived from the JSON
// error-output example in docs/design_doc.md (search "Error output");
// fields like statement_type that are subcommand-specific live in Data
// rather than in the envelope itself.
package output

import (
	"encoding/json"
	"errors"
)

// ErrRendered is returned by Renderer.RenderError after it has emitted
// an envelope describing the failure. main.go uses errors.Is to
// recognize this sentinel and suppress its default "Error: ..." stderr
// print, while still exiting with a non-zero status. This preserves the
// JSON-mode contract that stdout is the single source of truth and keeps
// agents from having to parse stderr.
var ErrRendered = errors.New("output: error already rendered as envelope")

// Tier identifies which analysis tier produced a response. The names
// and order match the "Three analysis tiers" section in
// docs/design_doc.md:
//
//	zero_config — Tier 1, expression type checking (zero-config).
//	schema_file — Tier 2, name resolution (requires schema files).
//	connected   — Tier 3, connected validation.
//
// TierUnset is the zero value and indicates the command has no tier
// semantics (e.g. version).
type Tier string

// Tier values.
const (
	TierUnset      Tier = ""
	TierZeroConfig Tier = "zero_config"
	TierSchemaFile Tier = "schema_file"
	TierConnected  Tier = "connected"
)

// ConnectionStatus reports whether the command reached a live cluster.
// Commands that never connect (version, parse, format) report
// ConnectionDisconnected.
type ConnectionStatus string

// ConnectionStatus values.
const (
	ConnectionDisconnected ConnectionStatus = "disconnected"
	ConnectionConnected    ConnectionStatus = "connected"
)

// Envelope is the response contract every crdb-sql subcommand returns
// under --output=json.
//
// Lifecycle: built fresh by the subcommand's RunE, marshalled once by
// Renderer.Render, then discarded. No long-lived state.
//
// Field discipline:
//   - Tier uses omitempty so commands without tier semantics (e.g.
//     version) omit the field rather than emitting a misleading empty
//     string. This relies on TierUnset being the empty-string zero
//     value.
//   - ParserVersion and ConnectionStatus are always emitted so agents
//     can rely on their presence.
//   - TargetVersion uses omitempty so the field is absent when the
//     user did not pass --target-version (or the equivalent MCP
//     parameter). This preserves byte-identical output for the
//     unflagged path.
//   - Errors uses omitempty so a clean run produces no errors key
//     rather than a noisy "errors": null.
//   - Data is json.RawMessage so each subcommand controls its own
//     payload marshalling and this package never depends on subcommand
//     types.
type Envelope struct {
	Tier             Tier             `json:"tier,omitempty"`
	ParserVersion    string           `json:"parser_version"`
	TargetVersion    string           `json:"target_version,omitempty"`
	ConnectionStatus ConnectionStatus `json:"connection_status"`
	Errors           []Error          `json:"errors,omitempty"`
	Data             json.RawMessage  `json:"data,omitempty"`
}

// Stable Error.Code values. Agents key off these strings, so renames
// are breaking changes; new codes are added by appending here. Codes
// are namespaced by the producing concept (e.g. "target_version_*"
// for envelope-level version-handling diagnostics).
const (
	// CodeTargetVersionMismatch is emitted by
	// output.VersionMismatchWarning when the user-declared target
	// version differs from the bundled parser version at the
	// MAJOR.MINOR level.
	CodeTargetVersionMismatch = "target_version_mismatch"
)

// Severity is the severity level of a structured Error. Values use the
// PostgreSQL frontend/backend protocol severity strings (uppercase) so
// wire output matches what agents see from a live cluster. Defining the
// set as named constants prevents subcommands from drifting between
// "ERROR"/"error"/"Error" as more error producers come online.
type Severity string

// Severity values.
const (
	SeverityError   Severity = "ERROR"
	SeverityWarning Severity = "WARNING"
	SeverityNotice  Severity = "NOTICE"
	SeverityFatal   Severity = "FATAL"
	SeverityPanic   Severity = "PANIC"
)

// Error is the per-error schema agents consume; the fields are derived
// from the JSON example in docs/design_doc.md (search "Error output"). Code,
// Severity, and Message are required (every error has them); Position,
// Category, Context, and Suggestions are optional so early subcommands
// can populate only what they have. Richer enrichment lands with later
// issues.
type Error struct {
	Code        string         `json:"code"`
	Severity    Severity       `json:"severity"`
	Message     string         `json:"message"`
	Position    *Position      `json:"position,omitempty"`
	Category    string         `json:"category,omitempty"`
	Context     map[string]any `json:"context,omitempty"`
	Suggestions []Suggestion   `json:"suggestions,omitempty"`
}

// Position is a 1-based line/column with a 0-based byte offset, matching
// the convention used by the CockroachDB parser.
type Position struct {
	Line       int `json:"line"`
	Column     int `json:"column"`
	ByteOffset int `json:"byte_offset"`
}

// Suggestion is a structured fix proposal: replace bytes [Range.Start,
// Range.End) with Replacement. Confidence is in [0, 1]; Reason is a
// machine-readable label (e.g. "levenshtein_distance_1").
type Suggestion struct {
	Replacement string  `json:"replacement"`
	Range       Range   `json:"range"`
	Confidence  float64 `json:"confidence"`
	Reason      string  `json:"reason"`
}

// Range is a half-open byte range [Start, End) into the original input.
type Range struct {
	Start int `json:"start"`
	End   int `json:"end"`
}
