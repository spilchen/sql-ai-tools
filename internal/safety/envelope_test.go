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

func TestEnvelopeSuggestionsByOp(t *testing.T) {
	// Both OpExplain and OpExplainDDL escalation suggestions point at
	// safe_write — the lowest-privilege mode that admits the call per
	// design doc §Safety Model. Jumping to full_access would violate
	// principle of least privilege, and the test pins the symmetry so
	// a future change can't accidentally regress only one op.
	//
	// Kind is set explicitly to KindSchema (the most common explain
	// rejection class) so the test exercises the same code path a
	// real classifyReadOnly call would produce — Violations from
	// production code never have Kind=0.
	tests := []struct {
		name string
		op   safety.Operation
	}{
		{name: "explain", op: safety.OpExplain},
		{name: "explain_ddl", op: safety.OpExplainDDL},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := safety.Envelope(&safety.Violation{
				Tag:  "ANY",
				Mode: safety.ModeReadOnly,
				Op:   tc.op,
				Kind: safety.KindSchema,
			})
			require.Len(t, e.Suggestions, 1)
			require.Equal(t, string(safety.ModeSafeWrite), e.Suggestions[0].Replacement)
			require.Equal(t, "safety_mode_escalation", e.Suggestions[0].Reason)
		})
	}
}

func TestEnvelopeNoSuggestionForUnimplementedModes(t *testing.T) {
	// OpExplain in safe_write/full_access still reports "not yet
	// implemented" (the explain-side mode wiring is tracked
	// separately). No escalation makes sense — the user has to wait
	// — so no suggestion is offered.
	e := safety.Envelope(&safety.Violation{
		Tag:  "SELECT",
		Mode: safety.ModeSafeWrite,
		Op:   safety.OpExplain,
		Kind: safety.KindUnimplemented,
	})
	require.Empty(t, e.Suggestions)
}

func TestEnvelopeExecuteSuggestions(t *testing.T) {
	// OpExecute escalation is asymmetric: a write under read_only
	// escalates to safe_write (the smallest bump), but schema changes
	// and DCL under read_only must jump to full_access because
	// safe_write itself rejects them. A safe_write rejection of
	// schema/DCL escalates to full_access — there's no intermediate
	// stop. The decision is driven by Violation.Kind, not by the
	// human-readable Reason text, so wording tweaks in the classifier
	// cannot silently break the escalation contract.
	tests := []struct {
		name                string
		mode                safety.Mode
		kind                safety.ViolationKind
		expectedReplacement safety.Mode
	}{
		{
			name:                "write under read_only suggests safe_write",
			mode:                safety.ModeReadOnly,
			kind:                safety.KindWrite,
			expectedReplacement: safety.ModeSafeWrite,
		},
		{
			name:                "schema change under read_only suggests full_access",
			mode:                safety.ModeReadOnly,
			kind:                safety.KindSchema,
			expectedReplacement: safety.ModeFullAccess,
		},
		{
			name:                "privilege change under read_only suggests full_access",
			mode:                safety.ModeReadOnly,
			kind:                safety.KindPrivilege,
			expectedReplacement: safety.ModeFullAccess,
		},
		{
			name:                "cluster admin under read_only suggests full_access",
			mode:                safety.ModeReadOnly,
			kind:                safety.KindClusterAdmin,
			expectedReplacement: safety.ModeFullAccess,
		},
		{
			name:                "schema change under safe_write suggests full_access",
			mode:                safety.ModeSafeWrite,
			kind:                safety.KindSchema,
			expectedReplacement: safety.ModeFullAccess,
		},
		{
			name:                "privilege change under safe_write suggests full_access",
			mode:                safety.ModeSafeWrite,
			kind:                safety.KindPrivilege,
			expectedReplacement: safety.ModeFullAccess,
		},
		{
			name:                "cluster admin under safe_write suggests full_access",
			mode:                safety.ModeSafeWrite,
			kind:                safety.KindClusterAdmin,
			expectedReplacement: safety.ModeFullAccess,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := safety.Envelope(&safety.Violation{
				Tag:  "ANY",
				Mode: tc.mode,
				Op:   safety.OpExecute,
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
	// unknown-mode programmer errors, and explain-DDL-with-non-DDL.
	// suggestionsFor must return no escalation hint for these so
	// agents don't burn a retry on a mode that won't help.
	tests := []struct {
		name string
		mode safety.Mode
		op   safety.Operation
		kind safety.ViolationKind
	}{
		{name: "nested explain", mode: safety.ModeReadOnly, op: safety.OpExecute, kind: safety.KindNestedExplain},
		{name: "empty input", mode: safety.ModeReadOnly, op: safety.OpExecute, kind: safety.KindOther},
		{name: "explain_ddl wrong input shape", mode: safety.ModeReadOnly, op: safety.OpExplainDDL, kind: safety.KindBadOpInput},
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
