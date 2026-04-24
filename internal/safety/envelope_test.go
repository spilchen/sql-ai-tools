// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package safety_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/safety"
)

func TestEnvelope(t *testing.T) {
	v := &safety.Violation{
		Tag:    "DROP TABLE",
		Reason: "statement modifies schema; read_only mode forbids it",
		Mode:   safety.ModeReadOnly,
		Op:     safety.OpExplain,
	}

	e := safety.Envelope(v)

	require.Equal(t, output.CodeSafetyViolation, e.Code)
	require.Equal(t, output.SeverityError, e.Severity)
	require.Equal(t, "safety", e.Category)
	require.Contains(t, e.Message, "DROP TABLE")
	require.Contains(t, e.Message, "read_only")
	require.Contains(t, e.Message, "explain")

	require.Equal(t, "DROP TABLE", e.Context["tag"])
	require.Equal(t, "read_only", e.Context["mode"])
	require.Equal(t, "explain", e.Context["operation"])
	require.NotEmpty(t, e.Context["reason"])
}

func TestEnvelopeExplainDDLSuggestion(t *testing.T) {
	// Under read_only/OpExplain, a KindSchema rejection escalates to
	// safe_write — the smallest mode that admits DDL on the
	// auto-dispatching explain_sql path (classifySafeWriteExplain
	// admits DDL since #167). This is asymmetric with OpExecute, where
	// KindSchema jumps to full_access because safe_write/OpExecute
	// rejects DDL. A regression suggesting full_access here would skip
	// the least-privilege step.
	e := safety.Envelope(&safety.Violation{
		Tag:  "CREATE TABLE",
		Mode: safety.ModeReadOnly,
		Op:   safety.OpExplain,
		Kind: safety.KindSchema,
	})
	require.Len(t, e.Suggestions, 1)
	require.Equal(t, string(safety.ModeSafeWrite), e.Suggestions[0].Replacement)
	require.Equal(t, "safety_mode_escalation", e.Suggestions[0].Reason)
}

func TestEnvelopeNoSuggestionForUnimplementedModes(t *testing.T) {
	// OpSimulate in safe_write/full_access still reports "not yet
	// implemented" (the OpSimulate mode wiring is tracked
	// separately). No escalation makes sense — the user has to
	// wait — so no suggestion is offered.
	e := safety.Envelope(&safety.Violation{
		Tag:  "SELECT",
		Mode: safety.ModeSafeWrite,
		Op:   safety.OpSimulate,
		Kind: safety.KindUnimplemented,
	})
	require.Empty(t, e.Suggestions)
}

func TestEnvelopeExecuteAndExplainSuggestions(t *testing.T) {
	// OpExecute and OpExplain share the escalation matrix for most
	// Kinds: writes go to safe_write, DCL/cluster-admin rejections jump
	// to full_access. They diverge for KindSchema because OpExplain
	// auto-dispatches DDL to EXPLAIN (DDL, SHAPE) and admits DDL under
	// safe_write (#167), while OpExecute reserves schema mutation for
	// full_access. The decision is driven by Violation.Kind (and Op
	// where they diverge), not by the human-readable Reason text, so
	// wording tweaks in the classifier cannot silently break the
	// escalation contract.
	cases := []struct {
		name                string
		mode                safety.Mode
		op                  safety.Operation
		kind                safety.ViolationKind
		expectedReplacement safety.Mode
	}{
		// Shared rows.
		{name: "execute write under read_only suggests safe_write",
			mode: safety.ModeReadOnly, op: safety.OpExecute, kind: safety.KindWrite,
			expectedReplacement: safety.ModeSafeWrite},
		{name: "explain write under read_only suggests safe_write",
			mode: safety.ModeReadOnly, op: safety.OpExplain, kind: safety.KindWrite,
			expectedReplacement: safety.ModeSafeWrite},
		{name: "execute privilege under read_only suggests full_access",
			mode: safety.ModeReadOnly, op: safety.OpExecute, kind: safety.KindPrivilege,
			expectedReplacement: safety.ModeFullAccess},
		{name: "explain privilege under read_only suggests full_access",
			mode: safety.ModeReadOnly, op: safety.OpExplain, kind: safety.KindPrivilege,
			expectedReplacement: safety.ModeFullAccess},
		{name: "execute cluster admin under read_only suggests full_access",
			mode: safety.ModeReadOnly, op: safety.OpExecute, kind: safety.KindClusterAdmin,
			expectedReplacement: safety.ModeFullAccess},
		{name: "explain cluster admin under read_only suggests full_access",
			mode: safety.ModeReadOnly, op: safety.OpExplain, kind: safety.KindClusterAdmin,
			expectedReplacement: safety.ModeFullAccess},
		{name: "execute privilege under safe_write suggests full_access",
			mode: safety.ModeSafeWrite, op: safety.OpExecute, kind: safety.KindPrivilege,
			expectedReplacement: safety.ModeFullAccess},
		{name: "explain privilege under safe_write suggests full_access",
			mode: safety.ModeSafeWrite, op: safety.OpExplain, kind: safety.KindPrivilege,
			expectedReplacement: safety.ModeFullAccess},
		{name: "execute cluster admin under safe_write suggests full_access",
			mode: safety.ModeSafeWrite, op: safety.OpExecute, kind: safety.KindClusterAdmin,
			expectedReplacement: safety.ModeFullAccess},
		{name: "explain cluster admin under safe_write suggests full_access",
			mode: safety.ModeSafeWrite, op: safety.OpExplain, kind: safety.KindClusterAdmin,
			expectedReplacement: safety.ModeFullAccess},

		// Divergent: OpExecute schema → full_access (safe_write rejects
		// DDL on execute), OpExplain schema → safe_write (auto-dispatch
		// admits DDL).
		{name: "execute schema under read_only suggests full_access",
			mode: safety.ModeReadOnly, op: safety.OpExecute, kind: safety.KindSchema,
			expectedReplacement: safety.ModeFullAccess},
		{name: "explain schema under read_only suggests safe_write",
			mode: safety.ModeReadOnly, op: safety.OpExplain, kind: safety.KindSchema,
			expectedReplacement: safety.ModeSafeWrite},
		{name: "execute schema under safe_write suggests full_access",
			mode: safety.ModeSafeWrite, op: safety.OpExecute, kind: safety.KindSchema,
			expectedReplacement: safety.ModeFullAccess},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := safety.Envelope(&safety.Violation{
				Tag:  "ANY",
				Mode: tc.mode,
				Op:   tc.op,
				Kind: tc.kind,
			})
			require.Len(t, e.Suggestions, 1)
			require.Equal(t, string(tc.expectedReplacement), e.Suggestions[0].Replacement)
			require.Equal(t, "safety_mode_escalation", e.Suggestions[0].Reason)
		})
	}
}

func TestEnvelopeNoSuggestionForUnactionableKinds(t *testing.T) {
	// Some rejections cannot be unblocked by mode escalation —
	// nested EXPLAIN wrappers, the empty-input defensive case,
	// unknown-mode programmer errors, and bad-input-shape rejections
	// (e.g. simulate's TCL/DCL no-route case). suggestionsFor must
	// return no escalation hint for these so agents don't burn a
	// retry on a mode that won't help.
	tests := []struct {
		name string
		mode safety.Mode
		op   safety.Operation
		kind safety.ViolationKind
	}{
		{name: "nested explain", mode: safety.ModeReadOnly, op: safety.OpExecute, kind: safety.KindNestedExplain},
		{name: "empty input", mode: safety.ModeReadOnly, op: safety.OpExecute, kind: safety.KindOther},
		{name: "simulate no-route input", mode: safety.ModeReadOnly, op: safety.OpSimulate, kind: safety.KindBadOpInput},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := safety.Envelope(&safety.Violation{
				Tag:  "ANY",
				Mode: tc.mode,
				Op:   tc.op,
				Kind: tc.kind,
			})
			require.Empty(t, e.Suggestions)
		})
	}
}
