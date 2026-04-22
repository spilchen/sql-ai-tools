// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package schemawarn bridges catalog loader warnings into the standard
// envelope error stream. Both the CLI subcommands that consume a
// catalog (validate, list-tables, describe) and the MCP tools that do
// the same need to surface non-fatal loader diagnostics (skipped
// statements, duplicate definitions) so agents see them alongside hard
// errors. Centralizing the conversion keeps the wire code stable —
// `schema_warning` — and avoids two surfaces drifting apart.
package schemawarn

import (
	"github.com/spilchen/sql-ai-tools/internal/catalog"
	"github.com/spilchen/sql-ai-tools/internal/output"
)

// Code is the envelope error Code emitted for every catalog warning.
// It is part of the wire contract: agents may filter or branch on it.
const Code = "schema_warning"

// Append copies any non-fatal issues recorded by the catalog loader
// into env as warning-severity envelope entries. Call this once after
// catalog.Load / catalog.LoadFiles. A nil cat is treated as "no
// catalog loaded" and leaves env unchanged so handlers don't have to
// guard a failed Load themselves; cat with no warnings is similarly
// a no-op.
func Append(env *output.Envelope, cat *catalog.Catalog) {
	if cat == nil {
		return
	}
	for _, w := range cat.Warnings() {
		env.Errors = append(env.Errors, output.Error{
			Code:     Code,
			Severity: output.SeverityWarning,
			Message:  w,
		})
	}
}
