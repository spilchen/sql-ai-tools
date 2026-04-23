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
			})
			require.Len(t, e.Suggestions, 1)
			require.Equal(t, string(safety.ModeSafeWrite), e.Suggestions[0].Replacement)
			require.Equal(t, "safety_mode_escalation", e.Suggestions[0].Reason)
		})
	}
}

func TestEnvelopeNoSuggestionForUnimplementedModes(t *testing.T) {
	// safe_write/full_access violations are "not implemented" — the
	// fix is to wait for issues #28/#29, not to escalate. So no
	// suggestion is offered.
	e := safety.Envelope(&safety.Violation{
		Tag:  "SELECT",
		Mode: safety.ModeSafeWrite,
		Op:   safety.OpExplain,
	})
	require.Empty(t, e.Suggestions)
}
