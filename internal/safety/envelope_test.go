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
	// classifyReadOnly's OpExplainDDL branch tags every reachable
	// rejection as KindSchema (KindBadOpInput is short-circuited at
	// the top of escalationTargetFor). safe_write is the smallest
	// mode that admits DDL on the explain-ddl path
	// (classifySafeWriteExplainDDL). This pins that contract — a
	// regression suggesting full_access here would skip the
	// least-privilege step.
	e := safety.Envelope(&safety.Violation{
		Tag:  "CREATE TABLE",
		Mode: safety.ModeReadOnly,
		Op:   safety.OpExplainDDL,
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
	// OpExecute and OpExplain share the same escalation matrix
	// (issue #151 wired OpExplain to mirror OpExecute's per-Kind
	// behavior). The matrix is asymmetric: a write under read_only
	// escalates to safe_write (the smallest bump), but schema changes
	// and DCL under read_only must jump to full_access because
	// safe_write itself rejects them. A safe_write rejection of
	// schema/DCL escalates to full_access — there's no intermediate
	// stop. The decision is driven by Violation.Kind, not by the
	// human-readable Reason text, so wording tweaks in the classifier
	// cannot silently break the escalation contract.
	//
	// Running the same matrix against both ops pins that any future
	// refactor that diverges OpExplain from OpExecute would surface
	// here — losing the parallel would silently break agents that
	// switch between the two surfaces for the same statement.
	cases := []struct {
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

	ops := []struct {
		name string
		op   safety.Operation
	}{
		{name: "execute", op: safety.OpExecute},
		{name: "explain", op: safety.OpExplain},
	}

	for _, op := range ops {
		for _, tc := range cases {
			t.Run(op.name+"/"+tc.name, func(t *testing.T) {
				e := safety.Envelope(&safety.Violation{
					Tag:  "ANY",
					Mode: tc.mode,
					Op:   op.op,
					Kind: tc.kind,
				})
				require.Len(t, e.Suggestions, 1)
				require.Equal(t, string(tc.expectedReplacement), e.Suggestions[0].Replacement)
				require.Equal(t, "safety_mode_escalation", e.Suggestions[0].Reason)
			})
		}
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
