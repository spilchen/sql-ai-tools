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
// OpSimulate dispatches each statement through a non-executing EXPLAIN
// flavour, so it admits DDL/DML/SELECT under read_only without ever
// reaching cluster execution. OpExecute, in contrast, admits the
// read-only set under read_only, adds DML under safe_write, and
// admits anything that parses under full_access — the full per-mode
// matrix in the design doc.
type Operation int

// Operation values. New surfaces extend this enum; the existing rules
// stay untouched.
const (
	OpExplain Operation = iota + 1
	OpExplainDDL
	OpSimulate
	OpExecute
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
	case OpExecute:
		return "execute"
	default:
		return "unknown"
	}
}

// ViolationKind labels the structural reason a statement was
// rejected, separate from the human-readable Reason text. Code that
// needs to act on the rejection (e.g. envelope.suggestionsFor picking
// the smallest mode that would admit the call) branches on Kind so
// the decision is not coupled to specific Reason wording.
type ViolationKind int

// ViolationKind values. Start at one so the zero value is invalid;
// Check always sets a meaningful kind on every Violation it produces.
const (
	// KindOther covers rejections that do not need fine-grained
	// classification — currently the empty-input defensive case, the
	// unknown-mode programmer-error case, and the unknown-Operation
	// programmer-error case in classifyReadOnly. None can be
	// unblocked by escalating mode, so suggestionsFor emits no
	// escalation hint for them.
	KindOther ViolationKind = iota + 1

	// KindWrite labels statements rejected because they would mutate
	// data (INSERT/UPDATE/UPSERT/DELETE/TRUNCATE). Escalating to
	// safe_write unblocks these.
	KindWrite

	// KindSchema labels statements rejected because they would
	// mutate schema (CREATE/ALTER/DROP). Escalating to full_access
	// unblocks these — safe_write does not.
	KindSchema

	// KindPrivilege labels privilege and identity statements
	// (GRANT/REVOKE, CREATE/DROP/ALTER ROLE, ownership and default-
	// privilege changes). The classic SQL Data Control Language set.
	// Escalating to full_access unblocks these — safe_write rejects
	// privilege management even though the parser also tags GRANT
	// and friends as schema-modifying.
	KindPrivilege

	// KindClusterAdmin labels statements that the parser also tags
	// TypeDCL but that aren't privilege/role changes — cluster
	// configuration (SET CLUSTER SETTING, SET TRACING), zone
	// configuration (ALTER ... CONFIGURE ZONE), and tenant lifecycle
	// (CREATE/DROP/ALTER TENANT). Treated as a separate Kind from
	// KindPrivilege so the rejection Reason names what the user is
	// actually doing rather than misleading them about a privilege
	// change. Escalates to full_access.
	KindClusterAdmin

	// KindNestedExplain labels EXPLAIN/EXPLAIN ANALYZE wrappers
	// rejected to prevent the inner statement from sneaking past
	// CanWriteData / CanModifySchema. Mode escalation does not help
	// — the user must pass the inner statement directly.
	KindNestedExplain

	// KindUnimplemented labels (mode, op) pairs that the package
	// recognises at the flag layer but does not yet wire (today:
	// safe_write/full_access for OpExplain/OpExplainDDL). The fix is
	// for the upstream feature to land, not for the user to escalate.
	KindUnimplemented

	// KindBadOpInput labels rejections caused by the user passing the
	// wrong shape of input for the operation — currently only
	// non-DDL statements to OpExplainDDL.
	KindBadOpInput
)

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
	// CRDB versions for any given statement type. Empty for the
	// empty-input defensive case (no statement to tag).
	Tag string

	// Reason is a short human-readable explanation of why the
	// statement was rejected (e.g. "writes data", "modifies schema",
	// "expected DDL"). Embedded into the rendered Message; agents
	// should branch on Kind/Mode/Operation/Tag rather than Reason.
	Reason string

	// Mode is the safety mode that was in effect when the violation
	// was raised. Lets agents recognise that escalating to a higher
	// mode would unblock the call.
	Mode Mode

	// Op is the cluster-bound surface that triggered the check. Lets
	// agents distinguish "explain rejected DROP TABLE" from
	// "explain-ddl rejected SELECT", which point at different fixes.
	Op Operation

	// Kind is the structural reason for the rejection. Used by
	// envelope.suggestionsFor to pick the smallest mode escalation
	// that would admit the call (or to skip the suggestion entirely
	// when no escalation helps).
	Kind ViolationKind
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
		// Tag is the literal "EMPTY" sentinel so formatMessage doesn't
		// render a stray empty parens cell ("(, mode=…, op=…)").
		return &Violation{
			Tag:    "EMPTY",
			Reason: "no statements parsed",
			Mode:   mode,
			Op:     op,
			Kind:   KindOther,
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
//
// Mode coverage is intentionally scoped per Operation:
//
//   - read_only is wired for every Op via classifyReadOnly.
//   - safe_write and full_access are wired only for OpExecute (issue
//     #29). OpExplain, OpExplainDDL, and OpSimulate in those modes
//     still return the "not yet implemented" violation; wiring them
//     is follow-up work so the other surfaces' mode story stays
//     stable while exec adopts the full safety model.
func classify(mode Mode, op Operation, stmt tree.Statement) *Violation {
	switch mode {
	case ModeReadOnly:
		return classifyReadOnly(op, stmt)
	case ModeSafeWrite:
		if op == OpExecute {
			return classifySafeWriteExecute(stmt)
		}
		return notYetImplemented(mode, op, stmt)
	case ModeFullAccess:
		if op == OpExecute {
			return classifyFullAccessExecute(stmt)
		}
		return notYetImplemented(mode, op, stmt)
	default:
		// Defensive: ParseMode should have rejected this earlier.
		// Treat as a violation rather than admitting unknown modes.
		return &Violation{
			Tag:    stmt.StatementTag(),
			Reason: fmt.Sprintf("unknown safety mode %q", mode),
			Mode:   mode,
			Op:     op,
			Kind:   KindOther,
		}
	}
}

// notYetImplemented builds the placeholder violation returned for
// (mode, op) pairs that ParseMode admits but Check does not yet wire.
// Centralised so the message text stays consistent across surfaces.
func notYetImplemented(mode Mode, op Operation, stmt tree.Statement) *Violation {
	return &Violation{
		Tag:    stmt.StatementTag(),
		Reason: fmt.Sprintf("safety mode %q is not yet implemented", mode),
		Mode:   mode,
		Op:     op,
		Kind:   KindUnimplemented,
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
//     safe_write or full_access to use explain-ddl. The reason text
//     names the escalation path so the rejection is actionable.
func classifyReadOnly(op Operation, stmt tree.Statement) *Violation {
	tag := stmt.StatementTag()
	switch op {
	case OpExplain, OpExecute:
		// OpExplain wraps the inner stmt in `EXPLAIN <stmt>` and
		// OpExecute runs it directly; both share the same read-only
		// admission set because either path that touches a write would
		// violate the read_only contract (EXPLAIN doesn't execute, but
		// nested EXPLAIN ANALYZE wrappers do — and OpExecute's runtime
		// is the very thing we're gating).
		//
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
				Kind:   KindNestedExplain,
			}
		}
		// Order matters: a statement can satisfy more than one of
		// these predicates, and the Kind we tag determines which
		// escalation suggestion the envelope emits. Picking the
		// *most-permissive-needed* mode keeps an agent from looping
		// through retries on a mode that would also reject the
		// statement.
		//
		// CRDB overloads TypeDCL beyond the SQL-standard "Data Control
		// Language" definition — it covers GRANT/REVOKE/role
		// management AND cluster configuration AND tenant lifecycle.
		// We split the two so the rejection Reason matches what the
		// user is actually doing. classifyDCL routes both cases.
		//
		// Three tenant lifecycle nodes — AlterTenantCapability,
		// AlterTenantReplication, CreateTenantFromReplication — are
		// tagged TypeDML rather than TypeDCL upstream. That's an
		// upstream classification quirk, not a deliberate policy
		// signal, so we route them through isTenantMgmtDMLStmt to
		// land in the cluster-admin gate alongside their DCL siblings.
		//
		// Empirical predicate matrix (verified against
		// cockroachdb-parser v0.26.2):
		//
		//   GRANT/REVOKE/role mgmt
		//                     : DCL + schema + write   → KindPrivilege    → full_access
		//   SET CLUSTER SETTING / SET TRACING
		//                     : DCL + write            → KindClusterAdmin → full_access
		//   ALTER ... CONFIGURE ZONE
		//                     : DCL + schema + write   → KindClusterAdmin → full_access
		//   CREATE/DROP/ALTER TENANT *
		//                     : DCL + write            → KindClusterAdmin → full_access
		//   ALTER VIRTUAL CLUSTER CAPABILITY
		//                     : DML                    → KindClusterAdmin → full_access
		//   ALTER VIRTUAL CLUSTER REPLICATION,
		//   CREATE VIRTUAL CLUSTER FROM REPLICATION
		//                     : DML + write            → KindClusterAdmin → full_access
		//   CREATE/DROP/ALTER TABLE etc.
		//                     : DDL + schema           → KindSchema       → full_access
		//   TRUNCATE          : DDL + schema + write   → KindSchema       → full_access
		//   INSERT/UPDATE/DELETE/UPSERT
		//                     : DML + write            → KindWrite        → safe_write
		if v := classifyDCL(stmt, tag, ModeReadOnly, op); v != nil {
			return v
		}
		// Tenant lifecycle nodes the parser tags TypeDML rather than
		// TypeDCL belong with the cluster-admin family, not the
		// generic write/schema branches below. Without this guard,
		// AlterTenantCapability — for which the parser marks neither
		// CanWriteData nor CanModifySchema — would slip past every
		// remaining check and be silently admitted under read_only.
		// AlterTenantReplication and CreateTenantFromReplication do
		// have CanWriteData=true, so they would still be rejected by
		// the branch below, but as KindWrite — pointing the agent at
		// safe_write, which would also reject them.
		if isTenantMgmtDMLStmt(stmt) {
			return &Violation{
				Tag:    tag,
				Reason: clusterAdminReason(stmt, ModeReadOnly),
				Mode:   ModeReadOnly,
				Op:     op,
				Kind:   KindClusterAdmin,
			}
		}
		if tree.CanModifySchema(stmt) {
			return &Violation{
				Tag:    tag,
				Reason: "statement modifies schema; read_only mode forbids it",
				Mode:   ModeReadOnly,
				Op:     op,
				Kind:   KindSchema,
			}
		}
		if tree.CanWriteData(stmt) {
			return &Violation{
				Tag:    tag,
				Reason: "statement writes data; read_only mode forbids it",
				Mode:   ModeReadOnly,
				Op:     op,
				Kind:   KindWrite,
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
				Kind:   KindBadOpInput,
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
			Kind:   KindSchema,
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
				Kind:   KindNestedExplain,
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
				Kind:   KindBadOpInput,
			}
		}
	default:
		return &Violation{
			Tag:    tag,
			Reason: fmt.Sprintf("unknown operation %v (programmer error)", op),
			Mode:   ModeReadOnly,
			Op:     op,
			Kind:   KindOther,
		}
	}
}

// classifySafeWriteExecute is the safe_write rule for OpExecute. It
// admits the read_only set plus DML (INSERT/UPDATE/UPSERT/DELETE)
// while still rejecting DDL and DCL.
//
//   - DDL: schema mutation is reserved for full_access. The reason
//     names the escalation path so the rejection is actionable.
//   - DCL: GRANT/REVOKE escape sql_safe_updates and can hand out
//     privileges that outlive the session, so they require the
//     explicit full_access opt-in.
//
// Defense-in-depth: Manager.Execute additionally sets
// `SET LOCAL sql_safe_updates = on` for safe_write so the cluster
// rejects unqualified UPDATE/DELETE at runtime, even though the AST
// allowlist admits them in principle.
func classifySafeWriteExecute(stmt tree.Statement) *Violation {
	tag := stmt.StatementTag()
	switch stmt.(type) {
	case *tree.Explain, *tree.ExplainAnalyze:
		// Same defense-in-depth as classifyReadOnly: the inner stmt of
		// an EXPLAIN node is invisible to CanWriteData/CanModifySchema,
		// so a wrapper around DDL would otherwise sneak through here.
		return &Violation{
			Tag:    tag,
			Reason: "nested EXPLAIN is not permitted; pass the inner statement directly",
			Mode:   ModeSafeWrite,
			Op:     OpExecute,
			Kind:   KindNestedExplain,
		}
	}
	// classifyDCL handles the TypeDCL surface (privilege/role +
	// cluster admin) with mode-appropriate Reason text. Both Kinds
	// it produces — KindPrivilege and KindClusterAdmin — escalate
	// to full_access; safe_write rejects both even though the parser
	// also tags GRANT and friends as schema-modifying.
	if v := classifyDCL(stmt, tag, ModeSafeWrite, OpExecute); v != nil {
		return v
	}
	// Tenant lifecycle DML nodes (see isTenantMgmtDMLStmt) require
	// full_access for the same reason their DCL siblings do — they
	// reshape cluster-level tenant state, not row data. Without this
	// guard, AlterTenantReplication and CreateTenantFromReplication
	// would be admitted as ordinary CanWriteData=true statements
	// (safe_write permits writes), and AlterTenantCapability — which
	// the parser marks neither CanWriteData nor CanModifySchema —
	// would fall through every remaining check.
	if isTenantMgmtDMLStmt(stmt) {
		return &Violation{
			Tag:    tag,
			Reason: clusterAdminReason(stmt, ModeSafeWrite),
			Mode:   ModeSafeWrite,
			Op:     OpExecute,
			Kind:   KindClusterAdmin,
		}
	}
	if tree.CanModifySchema(stmt) {
		return &Violation{
			Tag:    tag,
			Reason: "statement modifies schema; rerun with --mode=full_access",
			Mode:   ModeSafeWrite,
			Op:     OpExecute,
			Kind:   KindSchema,
		}
	}
	return nil
}

// classifyDCL produces the Violation for a TypeDCL statement, or nil
// if stmt isn't TypeDCL. It splits CRDB's overloaded TypeDCL set
// (privilege/role changes vs. cluster admin / tenant lifecycle) so
// the rejection Reason names the actual operation domain.
//
// The Reason wording is mode-specific: read_only's "forbids it"
// framing wouldn't make sense for safe_write (where the same
// statement is also rejected, but the user has already opted into
// some writes), so safe_write uses "rerun with --mode=full_access".
func classifyDCL(stmt tree.Statement, tag string, mode Mode, op Operation) *Violation {
	if stmt.StatementType() != tree.TypeDCL {
		return nil
	}
	if isClusterAdminStmt(stmt) {
		reason := clusterAdminReason(stmt, mode)
		return &Violation{
			Tag:    tag,
			Reason: reason,
			Mode:   mode,
			Op:     op,
			Kind:   KindClusterAdmin,
		}
	}
	return &Violation{
		Tag:    tag,
		Reason: "privilege/role changes require --mode=full_access",
		Mode:   mode,
		Op:     op,
		Kind:   KindPrivilege,
	}
}

// isClusterAdminStmt reports whether stmt is one of the TypeDCL nodes
// that isn't actually a privilege/role change. The set is enumerated
// rather than described by a predicate because the parser exposes no
// "is admin" classification — see cockroachdb-parser v0.26.2's
// pkg/sql/sem/tree/stmt.go for the full TypeDCL inventory.
//
// Defaulting to "privilege" (rather than "admin") is intentional: if
// a future CRDB version adds a new privilege-related TypeDCL node
// without updating this list, the agent gets the right escalation
// hint with a slightly less-specific Reason. If the new node is an
// admin one, this list is the only place to update.
func isClusterAdminStmt(stmt tree.Statement) bool {
	switch stmt.(type) {
	case *tree.SetZoneConfig,
		*tree.SetClusterSetting, *tree.SetTracing,
		*tree.CreateTenant, *tree.DropTenant,
		*tree.AlterTenantSetClusterSetting, *tree.AlterTenantRename,
		*tree.AlterTenantReset, *tree.AlterTenantService:
		return true
	}
	return false
}

// isTenantMgmtDMLStmt reports whether stmt is one of the tenant
// lifecycle nodes the parser tags TypeDML rather than TypeDCL. These
// belong in the cluster-admin gate alongside CreateTenant /
// DropTenant / AlterTenant* — the parser tag is an upstream
// classification quirk, not a deliberate choice that the safety
// model should honour. Kept as a sibling of isClusterAdminStmt (and
// not folded into it) so the invariant "isClusterAdminStmt is only
// consulted for TypeDCL nodes" — relied on by classifyDCL — stays
// intact.
//
// Without this list:
//   - AlterTenantCapability is silently admitted under read_only
//     (its CanWriteData and CanModifySchema both return false).
//   - AlterTenantReplication and CreateTenantFromReplication land at
//     KindWrite under read_only (admitted under safe_write), so the
//     escalation hint and Reason mislabel a tenant-lifecycle
//     operation as a row write.
//
// Invariant: every type in this switch must also appear in
// clusterAdminReason's switch. A type listed here without a matching
// reason case is not a security bug (the reason fallback still says
// "cluster administration requires full_access"), but the agent loses
// the tenant-specific Reason text. The two switches are kept
// physically adjacent so the coupling is easy to see.
func isTenantMgmtDMLStmt(stmt tree.Statement) bool {
	switch stmt.(type) {
	case *tree.AlterTenantCapability,
		*tree.AlterTenantReplication,
		*tree.CreateTenantFromReplication:
		return true
	}
	return false
}

// clusterAdminReason returns the Reason text for a cluster-admin
// rejection, tailored both to the mode (so the verbiage matches the
// rest of the classifier) and to the operation domain so an agent
// reading the envelope knows whether it's tweaking a zone, a cluster
// setting, tracing, or a tenant.
func clusterAdminReason(stmt tree.Statement, mode Mode) string {
	suffix := "rerun with --mode=full_access"
	if mode == ModeReadOnly {
		suffix = "read_only mode forbids it"
	}
	switch stmt.(type) {
	case *tree.SetZoneConfig:
		return "zone configuration changes require full_access; " + suffix
	case *tree.SetClusterSetting:
		return "cluster setting changes require full_access; " + suffix
	case *tree.SetTracing:
		return "tracing changes require full_access; " + suffix
	case *tree.CreateTenant, *tree.DropTenant,
		*tree.AlterTenantSetClusterSetting, *tree.AlterTenantRename,
		*tree.AlterTenantReset, *tree.AlterTenantService,
		*tree.AlterTenantCapability, *tree.AlterTenantReplication,
		*tree.CreateTenantFromReplication:
		return "tenant management requires full_access; " + suffix
	}
	// Defensive fallback — isClusterAdminStmt returned true so the
	// switch above should be exhaustive over the cluster-admin set;
	// a generic message keeps the user unblocked if the two lists
	// drift.
	return "cluster administration requires full_access; " + suffix
}

// classifyFullAccessExecute is the full_access rule for OpExecute.
// Per design doc §Safety Model, full_access admits anything that
// parses; defense-in-depth comes from the statement timeout enforced
// by Manager.Execute and (eventually) an audit log. Empty input is
// already rejected upstream by Check's defensive guard, so we have no
// special case to handle here.
func classifyFullAccessExecute(_ tree.Statement) *Violation {
	return nil
}
