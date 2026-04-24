// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package safety

import (
	"fmt"

	"github.com/spilchen/sql-ai-tools/internal/output"
)

// Envelope converts a Violation into the structured output.Error that
// CLI and MCP surfaces emit. Centralising the conversion guarantees
// that the two surfaces produce byte-identical error shapes for the
// same rejection — agents can rely on the Code, Severity, and Context
// keys regardless of how they invoked the tool.
//
// Context keys: tag, mode, operation, reason. These mirror Violation's
// fields but use the same lowercase wire tokens already used elsewhere
// in the envelope.
//
// Suggestions: when the violation can be unblocked by escalating the
// safety mode (read_only on either op), a single Suggestion entry
// points the agent at the higher-mode escape hatch. The Range is zero
// because the suggestion is a flag value, not a SQL edit; agents that
// only know how to apply byte-range edits will skip it harmlessly.
func Envelope(v *Violation) output.Error {
	return output.Error{
		Code:        output.CodeSafetyViolation,
		Severity:    output.SeverityError,
		Message:     formatMessage(v),
		Category:    "safety",
		Context:     contextMap(v),
		Suggestions: suggestionsFor(v),
	}
}

// formatMessage builds the human-readable Message embedded in the
// envelope. Format: "<reason> (<tag>, mode=<mode>, op=<op>)" so a
// single line carries the salient facts in CLI text mode where the
// Context map is not rendered.
func formatMessage(v *Violation) string {
	return fmt.Sprintf("safety violation: %s (%s, mode=%s, op=%s)",
		v.Reason, v.Tag, v.Mode, v.Op)
}

// contextMap returns the Context payload for the envelope error. Keys
// are lowercase, matching the convention used by
// CodeFeatureNotYetIntroduced (see internal/output/envelope.go).
func contextMap(v *Violation) map[string]any {
	return map[string]any{
		"tag":       v.Tag,
		"mode":      string(v.Mode),
		"operation": v.Op.String(),
		"reason":    v.Reason,
	}
}

// suggestionsFor proposes the higher-mode escape hatch when the
// violation can be unblocked by mode escalation. The decision uses
// Violation.Kind (the structural reason) rather than scanning the
// human-readable Reason string — that keeps the escalation logic
// independent of wording tweaks in classifyReadOnly /
// classifySafeWriteExecute.
//
// Principle: pick the smallest mode bump that would admit the call.
// Schema and DCL skip safe_write (which also rejects them) and go
// straight to full_access. Writes go to safe_write. Rejections that
// no escalation can fix (KindNestedExplain, KindUnimplemented,
// KindBadOpInput, KindOther) get no suggestion.
func suggestionsFor(v *Violation) []output.Suggestion {
	target, ok := escalationTargetFor(v)
	if !ok {
		return nil
	}
	return []output.Suggestion{{
		Replacement: string(target),
		Reason:      "safety_mode_escalation",
	}}
}

// escalationTargetFor returns the mode an agent should retry with
// when v is unblockable by escalation, and false otherwise. Lifted
// out of suggestionsFor so the escalation matrix is a single
// (Mode, Op, Kind) → Mode table that's easy to scan and update.
//
// Unactionable Kinds short-circuit at the top: nested-EXPLAIN
// wrappers, the empty-input defensive case, unimplemented (mode, op)
// pairs, and explain-DDL wrong-input-shape rejections all need the
// caller to fix the input or wait for upstream work, not to bump the
// mode. Producing a hint anyway would burn an agent's retry on a
// path that cannot succeed.
func escalationTargetFor(v *Violation) (Mode, bool) {
	switch v.Kind {
	case KindNestedExplain, KindUnimplemented, KindBadOpInput, KindOther:
		return "", false
	}
	switch v.Mode {
	case ModeReadOnly:
		switch v.Op {
		case OpExplain, OpExecute:
			// OpExplain and OpExecute share the same escalation
			// matrix because their safe_write/full_access
			// classifiers admit the same per-Kind set: writes go to
			// safe_write, schema/DCL skip straight to full_access.
			// Suggesting safe_write for a KindSchema rejection here
			// would loop the agent — the safe_write classifier also
			// rejects schema mutations.
			switch v.Kind {
			case KindWrite:
				return ModeSafeWrite, true
			case KindSchema, KindPrivilege, KindClusterAdmin:
				return ModeFullAccess, true
			}
		case OpExplainDDL:
			// classifyReadOnly's OpExplainDDL branch tags every
			// rejection that reaches here as KindSchema (the DDL
			// itself); KindBadOpInput from non-DDL inputs is
			// short-circuited at the top of this function. safe_write
			// is the smallest mode that admits DDL on the
			// explain-ddl path (per classifySafeWriteExplainDDL),
			// so the smallest-bump principle picks it.
			return ModeSafeWrite, true
		}
	case ModeSafeWrite:
		switch v.Op {
		case OpExplain, OpExecute:
			// Same shared matrix as the read_only arm above.
			switch v.Kind {
			case KindSchema, KindPrivilege, KindClusterAdmin:
				return ModeFullAccess, true
			}
		}
	}
	return "", false
}
