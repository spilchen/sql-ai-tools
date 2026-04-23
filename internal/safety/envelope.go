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
// violation is mode-driven. We always suggest the lowest-privilege
// mode that would admit the call — safe_write per design doc §Safety
// Model — rather than jumping straight to full_access, so agents
// follow the principle of least privilege.
func suggestionsFor(v *Violation) []output.Suggestion {
	if v.Mode != ModeReadOnly {
		return nil
	}
	switch v.Op {
	case OpExplain, OpExplainDDL:
		return []output.Suggestion{{
			Replacement: string(ModeSafeWrite),
			Reason:      "safety_mode_escalation",
		}}
	default:
		return nil
	}
}
