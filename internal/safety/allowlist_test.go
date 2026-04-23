// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package safety_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/safety"
)

func TestCheckReadOnlyExplain(t *testing.T) {
	tests := []struct {
		name           string
		sql            string
		expectedReject bool
		expectedTag    string
	}{
		// Read-only allowlist accepts.
		{name: "select", sql: "SELECT * FROM t"},
		{name: "select with where", sql: "SELECT id FROM t WHERE id = 1"},
		{name: "show", sql: "SHOW TABLES"},
		{name: "show create", sql: "SHOW CREATE TABLE t"},
		{name: "values", sql: "VALUES (1), (2)"},
		{name: "with cte", sql: "WITH cte AS (SELECT 1) SELECT * FROM cte"},

		// Read-only allowlist rejects DML writes.
		{name: "insert", sql: "INSERT INTO t VALUES (1)", expectedReject: true, expectedTag: "INSERT"},
		{name: "update", sql: "UPDATE t SET x = 1 WHERE id = 1", expectedReject: true, expectedTag: "UPDATE"},
		{name: "delete", sql: "DELETE FROM t WHERE id = 1", expectedReject: true, expectedTag: "DELETE"},
		{name: "truncate", sql: "TRUNCATE TABLE t", expectedReject: true, expectedTag: "TRUNCATE"},

		// Read-only allowlist rejects DDL.
		{name: "drop table", sql: "DROP TABLE users", expectedReject: true, expectedTag: "DROP TABLE"},
		{name: "create table", sql: "CREATE TABLE x (id INT PRIMARY KEY)", expectedReject: true, expectedTag: "CREATE TABLE"},
		{name: "alter table", sql: "ALTER TABLE x ADD COLUMN y INT", expectedReject: true, expectedTag: "ALTER TABLE"},

		// Read-only allowlist rejects DCL writes.
		{name: "grant", sql: "GRANT SELECT ON t TO bob", expectedReject: true, expectedTag: "GRANT"},
		{name: "revoke", sql: "REVOKE SELECT ON t FROM bob", expectedReject: true, expectedTag: "REVOKE"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := safety.Check(safety.ModeReadOnly, safety.OpExplain, tc.sql)
			require.NoError(t, err)

			if !tc.expectedReject {
				require.Nil(t, v, "expected statement to be admitted")
				return
			}
			require.NotNil(t, v, "expected statement to be rejected")
			require.Equal(t, tc.expectedTag, v.Tag)
			require.Equal(t, safety.ModeReadOnly, v.Mode)
			require.Equal(t, safety.OpExplain, v.Op)
			require.NotEmpty(t, v.Reason)
		})
	}
}

func TestCheckReadOnlyExplainDDL(t *testing.T) {
	// In read_only mode, OpExplainDDL rejects everything: DDL because
	// it modifies schema, non-DDL because explain-ddl requires a DDL
	// inner stmt. The two reject reasons differ — that distinction is
	// the load-bearing assertion below.
	tests := []struct {
		name           string
		sql            string
		expectedReason string
	}{
		{
			name:           "ddl rejected with escalation hint",
			sql:            "ALTER TABLE x ADD COLUMN y INT",
			expectedReason: "rerun with --mode=safe_write or --mode=full_access",
		},
		{
			name:           "create table rejected with escalation hint",
			sql:            "CREATE TABLE x (id INT PRIMARY KEY)",
			expectedReason: "rerun with --mode=safe_write or --mode=full_access",
		},
		{
			name:           "select rejected as non-ddl",
			sql:            "SELECT 1",
			expectedReason: "explain_ddl requires a DDL statement",
		},
		{
			name:           "insert rejected as non-ddl",
			sql:            "INSERT INTO t VALUES (1)",
			expectedReason: "explain_ddl requires a DDL statement",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := safety.Check(safety.ModeReadOnly, safety.OpExplainDDL, tc.sql)
			require.NoError(t, err)
			require.NotNil(t, v)
			require.Contains(t, v.Reason, tc.expectedReason)
		})
	}
}

func TestCheckRejectsUnimplementedModes(t *testing.T) {
	// safe_write and full_access for the non-execute surfaces still
	// report "not yet implemented" — only OpExecute (issue #29)
	// wires those modes today; OpExplain/OpExplainDDL/OpSimulate are
	// tracked separately as follow-up work.
	tests := []struct {
		name string
		mode safety.Mode
		op   safety.Operation
	}{
		{name: "safe_write OpExplain not yet implemented", mode: safety.ModeSafeWrite, op: safety.OpExplain},
		{name: "full_access OpExplain not yet implemented", mode: safety.ModeFullAccess, op: safety.OpExplain},
		{name: "safe_write OpExplainDDL not yet implemented", mode: safety.ModeSafeWrite, op: safety.OpExplainDDL},
		{name: "full_access OpExplainDDL not yet implemented", mode: safety.ModeFullAccess, op: safety.OpExplainDDL},
		{name: "safe_write OpSimulate not yet implemented", mode: safety.ModeSafeWrite, op: safety.OpSimulate},
		{name: "full_access OpSimulate not yet implemented", mode: safety.ModeFullAccess, op: safety.OpSimulate},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := safety.Check(tc.mode, tc.op, "SELECT 1")
			require.NoError(t, err)
			require.NotNil(t, v, "non-read_only modes are not admitted yet for explain ops")
			require.Contains(t, v.Reason, "not yet implemented")
			require.Equal(t, tc.mode, v.Mode)
		})
	}
}

func TestCheckReadOnlyExecute(t *testing.T) {
	// OpExecute under read_only mirrors OpExplain's read-only set:
	// SELECT/SHOW/etc. pass; writes, DDL, and DCL are rejected before
	// any cluster contact. Differs from TestCheckReadOnlyExplain in
	// the Op assertion AND in pinning the structural Kind on each
	// rejection so escalation logic in envelope.suggestionsFor stays
	// correct.
	tests := []struct {
		name           string
		sql            string
		expectedReject bool
		expectedTag    string
		expectedKind   safety.ViolationKind
	}{
		{name: "select", sql: "SELECT * FROM t"},
		{name: "show", sql: "SHOW TABLES"},
		{name: "values", sql: "VALUES (1), (2)"},
		{name: "with cte", sql: "WITH cte AS (SELECT 1) SELECT * FROM cte"},

		{name: "insert rejected", sql: "INSERT INTO t VALUES (1)", expectedReject: true, expectedTag: "INSERT", expectedKind: safety.KindWrite},
		{name: "update rejected", sql: "UPDATE t SET x = 1 WHERE id = 1", expectedReject: true, expectedTag: "UPDATE", expectedKind: safety.KindWrite},
		{name: "delete rejected", sql: "DELETE FROM t WHERE id = 1", expectedReject: true, expectedTag: "DELETE", expectedKind: safety.KindWrite},
		{name: "truncate rejected", sql: "TRUNCATE TABLE t", expectedReject: true, expectedTag: "TRUNCATE", expectedKind: safety.KindSchema},

		{name: "drop table rejected", sql: "DROP TABLE users", expectedReject: true, expectedTag: "DROP TABLE", expectedKind: safety.KindSchema},
		{name: "create table rejected", sql: "CREATE TABLE x (id INT PRIMARY KEY)", expectedReject: true, expectedTag: "CREATE TABLE", expectedKind: safety.KindSchema},

		// GRANT under read_only must be tagged KindPrivilege even
		// though the parser also reports it as schema-modifying. If
		// it landed as KindWrite or KindSchema the escalation hint
		// would suggest safe_write — which itself rejects privilege
		// changes — and the agent would loop. Pinning Kind here
		// closes that loop.
		{name: "grant rejected", sql: "GRANT SELECT ON t TO bob", expectedReject: true, expectedTag: "GRANT", expectedKind: safety.KindPrivilege},
		{name: "revoke rejected", sql: "REVOKE SELECT ON t FROM bob", expectedReject: true, expectedTag: "REVOKE", expectedKind: safety.KindPrivilege},
		{name: "create role rejected", sql: "CREATE ROLE alice", expectedReject: true, expectedTag: "CREATE ROLE", expectedKind: safety.KindPrivilege},

		// CRDB tags several non-privilege statements as TypeDCL
		// (cluster config, tracing, zone config, tenant lifecycle).
		// These get KindClusterAdmin and a domain-specific Reason
		// rather than the misleading "privilege/role changes" message
		// the SQL-standard reading of DCL would imply.
		{name: "set cluster setting tagged as cluster admin",
			sql:            "SET CLUSTER SETTING sql.defaults.distsql = 'on'",
			expectedReject: true, expectedTag: "SET CLUSTER SETTING", expectedKind: safety.KindClusterAdmin},
		{name: "set tracing tagged as cluster admin",
			sql:            "SET TRACING = on",
			expectedReject: true, expectedTag: "SET TRACING", expectedKind: safety.KindClusterAdmin},
		{name: "configure zone tagged as cluster admin",
			sql:            "ALTER TABLE t CONFIGURE ZONE USING num_replicas = 5",
			expectedReject: true, expectedTag: "CONFIGURE ZONE", expectedKind: safety.KindClusterAdmin},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := safety.Check(safety.ModeReadOnly, safety.OpExecute, tc.sql)
			require.NoError(t, err)

			if !tc.expectedReject {
				require.Nil(t, v, "expected statement to be admitted")
				return
			}
			require.NotNil(t, v, "expected statement to be rejected")
			require.Equal(t, tc.expectedTag, v.Tag)
			require.Equal(t, safety.ModeReadOnly, v.Mode)
			require.Equal(t, safety.OpExecute, v.Op)
			require.Equal(t, tc.expectedKind, v.Kind)
		})
	}
}

func TestCheckReadOnlyExecuteNestedExplain(t *testing.T) {
	// The nested-EXPLAIN guard is shared between OpExplain and
	// OpExecute (same case arm in classifyReadOnly). A future
	// refactor that splits the case must not silently drop OpExecute
	// coverage; this test pins it.
	v, err := safety.Check(safety.ModeReadOnly, safety.OpExecute,
		"EXPLAIN ANALYZE INSERT INTO t VALUES (1)")
	require.NoError(t, err)
	require.NotNil(t, v)
	require.Contains(t, v.Reason, "nested EXPLAIN")
	require.Equal(t, safety.KindNestedExplain, v.Kind)
}

func TestCheckSafeWriteExecute(t *testing.T) {
	// safe_write admits the read-only set plus DML, but still rejects
	// DDL (with a full_access escalation hint) and DCL. The cluster's
	// sql_safe_updates session var is the runtime guard against
	// unqualified UPDATE/DELETE — that's wired in conn.Manager.Execute,
	// not in Check, so even bare-WHERE-less UPDATEs pass the AST gate.
	tests := []struct {
		name           string
		sql            string
		expectedReject bool
		expectedReason string
	}{
		{name: "select admitted", sql: "SELECT 1"},
		{name: "insert admitted", sql: "INSERT INTO t VALUES (1)"},
		{name: "update admitted", sql: "UPDATE t SET x = 1 WHERE id = 1"},
		{name: "delete admitted", sql: "DELETE FROM t WHERE id = 1"},
		{name: "upsert admitted", sql: "UPSERT INTO t VALUES (1)"},
		{name: "unqualified update admitted at AST layer",
			sql: "UPDATE t SET x = 1"},

		{name: "create table rejected with full_access hint",
			sql:            "CREATE TABLE x (id INT PRIMARY KEY)",
			expectedReject: true,
			expectedReason: "rerun with --mode=full_access"},
		{name: "drop table rejected",
			sql:            "DROP TABLE users",
			expectedReject: true,
			expectedReason: "rerun with --mode=full_access"},
		{name: "grant rejected as privilege change",
			sql:            "GRANT SELECT ON t TO bob",
			expectedReject: true,
			expectedReason: "privilege/role changes require --mode=full_access"},
		{name: "configure zone rejected as cluster admin",
			sql:            "ALTER TABLE t CONFIGURE ZONE USING num_replicas = 5",
			expectedReject: true,
			expectedReason: "zone configuration changes require full_access"},
		{name: "set cluster setting rejected as cluster admin",
			sql:            "SET CLUSTER SETTING sql.defaults.distsql = 'on'",
			expectedReject: true,
			expectedReason: "cluster setting changes require full_access"},
		{name: "set tracing rejected as cluster admin",
			sql:            "SET TRACING = on",
			expectedReject: true,
			expectedReason: "tracing changes require full_access"},
		{name: "explain analyze ddl rejected as nested",
			sql:            "EXPLAIN ANALYZE ALTER TABLE t ADD COLUMN x INT",
			expectedReject: true,
			expectedReason: "nested EXPLAIN"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := safety.Check(safety.ModeSafeWrite, safety.OpExecute, tc.sql)
			require.NoError(t, err)
			if !tc.expectedReject {
				require.Nil(t, v, "expected statement to be admitted")
				return
			}
			require.NotNil(t, v)
			require.Equal(t, safety.ModeSafeWrite, v.Mode)
			require.Equal(t, safety.OpExecute, v.Op)
			require.Contains(t, v.Reason, tc.expectedReason)
		})
	}
}

func TestCheckFullAccessExecute(t *testing.T) {
	// full_access admits anything that parses; defense-in-depth comes
	// from the statement timeout (and eventually an audit log), not
	// from the AST allowlist. Empty input is still rejected by Check's
	// defensive guard — that's covered by TestCheckRejectsEmptyInput.
	tests := []struct {
		name string
		sql  string
	}{
		{name: "select", sql: "SELECT 1"},
		{name: "insert", sql: "INSERT INTO t VALUES (1)"},
		{name: "drop table", sql: "DROP TABLE users"},
		{name: "create table", sql: "CREATE TABLE x (id INT PRIMARY KEY)"},
		{name: "grant", sql: "GRANT SELECT ON t TO bob"},
		{name: "multi statement", sql: "SELECT 1; INSERT INTO t VALUES (1)"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := safety.Check(safety.ModeFullAccess, safety.OpExecute, tc.sql)
			require.NoError(t, err)
			require.Nil(t, v, "full_access admits any parsed statement")
		})
	}
}

func TestCheckMultiStatementShortCircuits(t *testing.T) {
	// First statement is fine, second is a write — Check must reject
	// at the first violation and report the offending tag, not the
	// last admitted one.
	v, err := safety.Check(safety.ModeReadOnly, safety.OpExplain,
		"SELECT 1; DELETE FROM t WHERE id = 1; SELECT 2")
	require.NoError(t, err)
	require.NotNil(t, v)
	require.Equal(t, "DELETE", v.Tag)
}

func TestCheckParseErrorPropagates(t *testing.T) {
	// A malformed input must surface as a parse error, not a safety
	// violation: the user gets a real syntax diagnostic and not a
	// misleading "rejected by allowlist" message.
	v, err := safety.Check(safety.ModeReadOnly, safety.OpExplain, "SELEKT broken")
	require.Error(t, err)
	require.Nil(t, v)
}

func TestCheckRejectsEmptyInput(t *testing.T) {
	// parser.Parse("") returns zero stmts and no error, which without
	// an explicit empty-batch guard would be (nil, nil) — i.e.
	// "permitted". Pin the defensive rejection across every Op so a
	// regression that removes the guard is loud, and pin the Tag
	// sentinel so the rendered Message doesn't contain a stray empty
	// parens cell ("(, mode=…, op=…)").
	tests := []struct {
		name string
		sql  string
		op   safety.Operation
	}{
		{name: "explain empty string", sql: "", op: safety.OpExplain},
		{name: "explain whitespace only", sql: "   \n\t  ", op: safety.OpExplain},
		{name: "explain comment only", sql: "-- nothing here", op: safety.OpExplain},
		{name: "execute empty string", sql: "", op: safety.OpExecute},
		{name: "execute whitespace only", sql: "   \n\t  ", op: safety.OpExecute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := safety.Check(safety.ModeReadOnly, tc.op, tc.sql)
			require.NoError(t, err)
			require.NotNil(t, v, "empty input must not bypass the gate")
			require.Contains(t, v.Reason, "no statements parsed")
			require.Equal(t, "EMPTY", v.Tag, "empty-input Tag must use the EMPTY sentinel")
			require.Equal(t, safety.KindOther, v.Kind)

			// No escalation hint helps an empty input; pin that no
			// suggestion leaks out via Envelope.
			env := safety.Envelope(v)
			require.Empty(t, env.Suggestions,
				"empty input must not produce a mode-escalation hint")
			require.NotContains(t, env.Message, "(,", "Message must not contain an empty parens cell")
		})
	}
}

func TestCheckReadOnlySimulate(t *testing.T) {
	// OpSimulate dispatches to a non-executing EXPLAIN flavor for
	// each supported statement class, so read_only mode admits SELECT,
	// DML writes, and DDL alike. The rejections are scoped to shapes
	// the dispatcher has no route for: TCL, DCL, and nested EXPLAIN.
	tests := []struct {
		name           string
		sql            string
		expectedReject bool
		expectedReason string
	}{
		// Dispatchable shapes are admitted.
		{name: "select", sql: "SELECT * FROM t"},
		{name: "select with cte", sql: "WITH cte AS (SELECT 1) SELECT * FROM cte"},
		{name: "values", sql: "VALUES (1), (2)"},
		{name: "insert", sql: "INSERT INTO t VALUES (1)"},
		{name: "update", sql: "UPDATE t SET x = 1 WHERE id = 1"},
		{name: "delete", sql: "DELETE FROM t WHERE id = 1"},
		{name: "upsert", sql: "UPSERT INTO t VALUES (1)"},
		{name: "create table", sql: "CREATE TABLE x (id INT PRIMARY KEY)"},
		{name: "alter table add column", sql: "ALTER TABLE x ADD COLUMN y INT"},
		{name: "drop table", sql: "DROP TABLE x"},
		{name: "create index", sql: "CREATE INDEX i ON t (c)"},

		// TCL has no EXPLAIN form.
		{
			name:           "begin rejected",
			sql:            "BEGIN",
			expectedReject: true,
			expectedReason: "no route",
		},
		{
			name:           "commit rejected",
			sql:            "COMMIT",
			expectedReject: true,
			expectedReason: "no route",
		},

		// DCL is out of scope for the dispatcher.
		{
			name:           "grant rejected",
			sql:            "GRANT SELECT ON t TO bob",
			expectedReject: true,
			expectedReason: "no route",
		},
		{
			name:           "revoke rejected",
			sql:            "REVOKE SELECT ON t FROM bob",
			expectedReject: true,
			expectedReason: "no route",
		},

		// Nested EXPLAIN wrappers are rejected with the same reason
		// OpExplain uses, so a caller migrating between operations
		// gets a consistent message.
		{
			name:           "nested explain rejected",
			sql:            "EXPLAIN SELECT 1",
			expectedReject: true,
			expectedReason: "nested EXPLAIN",
		},
		{
			name:           "nested explain analyze rejected",
			sql:            "EXPLAIN ANALYZE INSERT INTO t VALUES (1)",
			expectedReject: true,
			expectedReason: "nested EXPLAIN",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := safety.Check(safety.ModeReadOnly, safety.OpSimulate, tc.sql)
			require.NoError(t, err)

			if !tc.expectedReject {
				require.Nil(t, v, "expected statement to be admitted under OpSimulate")
				return
			}
			require.NotNil(t, v, "expected statement to be rejected")
			require.Equal(t, safety.ModeReadOnly, v.Mode)
			require.Equal(t, safety.OpSimulate, v.Op)
			require.Contains(t, v.Reason, tc.expectedReason)
		})
	}
}

func TestOperationStringIncludesSimulate(t *testing.T) {
	// Operation.String is the wire-stable token agents branch on.
	// Pin every value so a future enum addition that forgets to
	// extend the switch (returning "unknown") fails loudly.
	require.Equal(t, "explain", safety.OpExplain.String())
	require.Equal(t, "explain_ddl", safety.OpExplainDDL.String())
	require.Equal(t, "simulate", safety.OpSimulate.String())
}

func TestCheckRejectsNestedExplain(t *testing.T) {
	// tree.CanWriteData/CanModifySchema do not descend into
	// *Explain/*ExplainAnalyze AST nodes, so a caller wrapping their
	// write in EXPLAIN ANALYZE would otherwise sneak through the AST
	// allowlist (the cluster's BEGIN READ ONLY catches it at runtime,
	// but defense-in-depth says reject before any cluster contact).
	tests := []struct {
		name string
		sql  string
	}{
		{name: "explain analyze write", sql: "EXPLAIN ANALYZE INSERT INTO t VALUES (1)"},
		{name: "explain analyze ddl", sql: "EXPLAIN ANALYZE ALTER TABLE t ADD COLUMN x INT"},
		{name: "explain select", sql: "EXPLAIN SELECT 1"},
		{name: "explain analyze select", sql: "EXPLAIN ANALYZE SELECT 1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := safety.Check(safety.ModeReadOnly, safety.OpExplain, tc.sql)
			require.NoError(t, err)
			require.NotNil(t, v, "nested EXPLAIN must be rejected at the AST layer")
			require.Contains(t, v.Reason, "nested EXPLAIN")
		})
	}
}
