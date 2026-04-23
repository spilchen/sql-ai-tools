// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package safety

import (
	"fmt"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/tree"
)

// Operation identifies the cluster-bound surface that is about to run
// the user's SQL. The allowlist applies a different rule per
// (Mode, Operation) pair — for example, OpExplainDDL must accept DDL
// (otherwise the underlying EXPLAIN (DDL, SHAPE) would always error),
// while OpExplain must reject it (because the inner statement of plain
// EXPLAIN is what defines the read/write character of the call).
type Operation int

// Operation values. New surfaces (e.g. an eventual OpExecute for
// issue #29) extend this enum; the existing rules stay untouched.
const (
	OpExplain Operation = iota + 1
	OpExplainDDL
	OpSimulate
)

// String returns the wire-stable token for op. Used in violation
// payloads so agents can branch on operation programmatically.
func (op Operation) String() string {
	switch op {
	case OpExplain:
		return "explain"
	case OpExplainDDL:
		return "explain_ddl"
	case OpSimulate:
		return "simulate"
	default:
		return "unknown"
	}
}

// Violation is the structured payload returned by Check when a
// statement is rejected. It carries everything an agent (or the
// envelope helper) needs to render an actionable error without
// reparsing the input.
//
// Lifecycle: built once by Check, consumed once by Envelope (for the
// CLI/MCP path) or by tests. No long-lived state.
type Violation struct {
	// Tag is the cockroachdb-parser StatementTag for the offending
	// statement (e.g. "DROP TABLE", "INSERT", "SELECT"). Stable across
	// CRDB versions for any given statement type.
	Tag string

	// Reason is a short human-readable explanation of why the
	// statement was rejected (e.g. "writes data", "modifies schema",
	// "expected DDL"). Embedded into the rendered Message; agents
	// should branch on Mode/Operation/Tag rather than Reason.
	Reason string

	// Mode is the safety mode that was in effect when the violation
	// was raised. Lets agents recognise that escalating to a higher
	// mode would unblock the call.
	Mode Mode

	// Op is the cluster-bound surface that triggered the check. Lets
	// agents distinguish "explain rejected DROP TABLE" from
	// "explain-ddl rejected SELECT", which point at different fixes.
	Op Operation
}

// Check parses sql and decides whether every statement in it is
// permitted under (mode, op). It is the single decision point for the
// allowlist: every Tier 3 surface (CLI, MCP) calls Check before opening
// a connection.
//
// The check is pure — no I/O, no cluster access — so it is cheap to
// run on every request and easy to unit-test. The same parser used
// elsewhere in crdb-sql (parser.Parse) is used here, so the
// classification cannot drift from what downstream consumers see.
//
// Return contract (the first three cases are mutually exclusive — Check
// never returns both a non-nil *Violation and a non-nil error):
//
//   - (nil, nil) — sql parses to one or more statements and every one
//     is permitted.
//   - (nil, err) — sql failed to parse. The caller should surface this
//     as a parse-error diagnostic (diag.FromParseError) rather than as
//     a safety violation: the input is malformed, not denied. The
//     error is the raw parser error so SQLSTATE and position survive.
//   - (*Violation, nil) — sql parses but is denied. Either the first
//     offending statement triggers a rule (multi-statement inputs
//     short-circuit on the first reject) or the batch is empty (a
//     defensive case so empty input cannot bypass the gate).
//
// Multi-statement inputs surface the first violation. This matches the
// "fail closed" discipline: if any statement in a batch would be
// rejected, the batch is rejected. Agents that care which statement
// failed can read Violation.Tag and re-issue individually.
func Check(mode Mode, op Operation, sql string) (*Violation, error) {
	stmts, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}
	if len(stmts) == 0 {
		// Defensive: parser.Parse("") returns zero stmts and no error.
		// The CLI's sqlinput layer rejects empty input upstream, but
		// the safety package's contract is "the single decision point"
		// — keep the gate self-contained so a future caller that
		// bypasses sqlinput cannot pass an empty batch through.
		return &Violation{
			Reason: "no statements parsed",
			Mode:   mode,
			Op:     op,
		}, nil
	}
	for _, s := range stmts {
		if v := classify(mode, op, s.AST); v != nil {
			return v, nil
		}
	}
	return nil, nil
}

// classify applies the (mode, op) rule to a single parsed statement.
// Returns nil when the statement is permitted; a populated *Violation
// otherwise. Pulled out of Check so the per-statement decision tree
// stays readable as new (mode, op) combinations are added.
func classify(mode Mode, op Operation, stmt tree.Statement) *Violation {
	switch mode {
	case ModeReadOnly:
		return classifyReadOnly(op, stmt)
	case ModeSafeWrite, ModeFullAccess:
		// Modes are recognised at the flag layer (so the CLI accepts
		// --mode=safe_write today without a "bad value" error), but
		// no statement is admitted until issue #29 wires the
		// per-mode rules. Returning a violation here keeps the
		// rejection path uniform with read_only.
		return &Violation{
			Tag:    stmt.StatementTag(),
			Reason: fmt.Sprintf("safety mode %q is not yet implemented", mode),
			Mode:   mode,
			Op:     op,
		}
	default:
		// Defensive: ParseMode should have rejected this earlier.
		// Treat as a violation rather than admitting unknown modes.
		return &Violation{
			Tag:    stmt.StatementTag(),
			Reason: fmt.Sprintf("unknown safety mode %q", mode),
			Mode:   mode,
			Op:     op,
		}
	}
}

// classifyReadOnly is the read-only-mode rule. The decision differs by
// Operation because the cluster-side wrapper changes what "read-only"
// means for the inner statement:
//
//   - OpExplain runs `EXPLAIN <inner>`. EXPLAIN does not execute the
//     inner statement, but agents still benefit from the inner-stmt
//     guard: it prevents nested EXPLAIN/EXPLAIN ANALYZE wrappers from
//     bypassing the AST allowlist (since tree.CanWriteData and
//     tree.CanModifySchema do not descend into *Explain/*ExplainAnalyze
//     AST nodes — an `EXPLAIN ANALYZE INSERT ...` would otherwise look
//     read-only at the top level). For the same reason, we reject any
//     inner statement that itself writes data or modifies schema. The
//     final allowlist matches the design doc's read-only set
//     (SELECT/SHOW/etc.).
//
//   - OpExplainDDL runs `EXPLAIN (DDL, SHAPE) <inner>`. The inner
//     statement must be DDL by construction, so a SELECT here is a
//     user error. But because read_only mode is meant to forbid all
//     schema modification, we reject DDL too — leaving OpExplainDDL
//     effectively unusable in read_only mode. Users must escalate to
//     safe_write or full_access (issue #29) to use explain-ddl.
//     The reason text names the escalation path so the rejection is
//     actionable.
//
//   - OpSimulate dispatches per-statement to a non-executing EXPLAIN
//     flavor (EXPLAIN ANALYZE for SELECT, plain EXPLAIN for DML
//     writes, EXPLAIN (DDL, SHAPE) for DDL). Because the dispatcher
//     never executes the inner write or DDL at the cluster level,
//     read_only mode admits every dispatched class — SELECT, DML
//     writes, and DDL alike — and only rejects shapes the dispatcher
//     has no route for: nested EXPLAIN (defense in depth, mirroring
//     OpExplain), TCL (BEGIN/COMMIT/ROLLBACK have no EXPLAIN form),
//     and DCL (GRANT/REVOKE, out of scope for the dispatcher).
func classifyReadOnly(op Operation, stmt tree.Statement) *Violation {
	tag := stmt.StatementTag()
	switch op {
	case OpExplain:
		// Reject nested EXPLAIN wrappers explicitly. The inner stmt of
		// an *Explain or *ExplainAnalyze node is not classified by
		// tree.CanWriteData/CanModifySchema, so without this branch a
		// caller could submit `EXPLAIN ANALYZE INSERT ...` and have it
		// admitted as read-only at the AST layer. The cluster's
		// BEGIN READ ONLY wrapper would still catch the write at
		// runtime (SQLSTATE 25006), but the AST allowlist's job is to
		// reject before any cluster contact.
		switch stmt.(type) {
		case *tree.Explain, *tree.ExplainAnalyze:
			return &Violation{
				Tag:    tag,
				Reason: "nested EXPLAIN is not permitted; pass the inner statement directly",
				Mode:   ModeReadOnly,
				Op:     op,
			}
		}
		if tree.CanWriteData(stmt) {
			return &Violation{
				Tag:    tag,
				Reason: "statement writes data; read_only mode forbids it",
				Mode:   ModeReadOnly,
				Op:     op,
			}
		}
		if tree.CanModifySchema(stmt) {
			return &Violation{
				Tag:    tag,
				Reason: "statement modifies schema; read_only mode forbids it",
				Mode:   ModeReadOnly,
				Op:     op,
			}
		}
		return nil
	case OpExplainDDL:
		if stmt.StatementType() != tree.TypeDDL {
			return &Violation{
				Tag:    tag,
				Reason: "explain_ddl requires a DDL statement",
				Mode:   ModeReadOnly,
				Op:     op,
			}
		}
		// Inner stmt is DDL, so it modifies schema. read_only mode
		// forbids all schema modification, including planning.
		// Surface the escalation path in the reason so the rejection
		// is actionable.
		return &Violation{
			Tag:    tag,
			Reason: "explain_ddl modifies schema; rerun with --mode=safe_write or --mode=full_access",
			Mode:   ModeReadOnly,
			Op:     op,
		}
	case OpSimulate:
		// Reject nested EXPLAIN wrappers explicitly. The dispatcher
		// classifies the inner statement to pick a route, but
		// tree.CanWriteData/CanModifySchema do not descend into
		// *Explain/*ExplainAnalyze AST nodes — so a wrapped write
		// would otherwise be misclassified as a SELECT and dispatched
		// to EXPLAIN ANALYZE, which executes. Reject the nested form
		// before any cluster contact and tell the caller to unwrap.
		switch stmt.(type) {
		case *tree.Explain, *tree.ExplainAnalyze:
			return &Violation{
				Tag:    tag,
				Reason: "nested EXPLAIN is not permitted; pass the inner statement directly",
				Mode:   ModeReadOnly,
				Op:     op,
			}
		}
		switch stmt.StatementType() {
		case tree.TypeDDL, tree.TypeDML:
			// Dispatcher has a route: DDL → EXPLAIN (DDL, SHAPE),
			// DML write → EXPLAIN, SELECT → EXPLAIN ANALYZE. None of
			// these execute the inner write or DDL at the cluster
			// level, so read_only mode admits them.
			return nil
		default:
			// TCL (BEGIN/COMMIT/ROLLBACK/SAVEPOINT) has no EXPLAIN
			// form. DCL (GRANT/REVOKE) is out of scope for the
			// dispatcher today. Both surface the same actionable
			// reason so callers can branch on Tag.
			return &Violation{
				Tag:    tag,
				Reason: "simulate has no route for this statement type; only DDL, DML, and SELECT are supported",
				Mode:   ModeReadOnly,
				Op:     op,
			}
		}
	default:
		return &Violation{
			Tag:    tag,
			Reason: fmt.Sprintf("unknown operation %v", op),
			Mode:   ModeReadOnly,
			Op:     op,
		}
	}
}
