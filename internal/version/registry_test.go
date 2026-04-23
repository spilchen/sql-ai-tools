// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package version

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSupports_BoundaryVersions exercises each seeded feature at the
// minor release just before its introduction, at exact introduction,
// and one minor after — the boundaries called out by the issue. A
// bug in compareMajorMinor would show up here first.
func TestSupports_BoundaryVersions(t *testing.T) {
	reg := DefaultRegistry()

	tests := []struct {
		name           string
		tag            string
		target         string
		expectedStatus Status
	}{
		{name: "plpgsql before", tag: FeaturePLpgSQLFunctionBody, target: "23.2", expectedStatus: StatusNotYetIntroduced},
		{name: "plpgsql at", tag: FeaturePLpgSQLFunctionBody, target: "24.1", expectedStatus: StatusSupported},
		{name: "plpgsql after", tag: FeaturePLpgSQLFunctionBody, target: "24.2", expectedStatus: StatusSupported},
		{name: "plpgsql with patch", tag: FeaturePLpgSQLFunctionBody, target: "24.1.3", expectedStatus: StatusSupported},
		{name: "plpgsql with v prefix", tag: FeaturePLpgSQLFunctionBody, target: "v24.1", expectedStatus: StatusSupported},

		{name: "trigram before", tag: FeatureTrigramIndex, target: "22.2", expectedStatus: StatusNotYetIntroduced},
		{name: "trigram at", tag: FeatureTrigramIndex, target: "23.1", expectedStatus: StatusSupported},
		{name: "trigram after", tag: FeatureTrigramIndex, target: "23.2", expectedStatus: StatusSupported},

		{name: "rbr before", tag: FeatureRegionalByRow, target: "20.2", expectedStatus: StatusNotYetIntroduced},
		{name: "rbr at", tag: FeatureRegionalByRow, target: "21.1", expectedStatus: StatusSupported},
		{name: "rbr after", tag: FeatureRegionalByRow, target: "21.2", expectedStatus: StatusSupported},

		{name: "alter changefeed before", tag: FeatureAlterChangefeed, target: "21.2", expectedStatus: StatusNotYetIntroduced},
		{name: "alter changefeed at", tag: FeatureAlterChangefeed, target: "22.1", expectedStatus: StatusSupported},
		{name: "alter changefeed after", tag: FeatureAlterChangefeed, target: "22.2", expectedStatus: StatusSupported},

		{name: "triggers before", tag: FeatureTriggers, target: "24.2", expectedStatus: StatusNotYetIntroduced},
		{name: "triggers at", tag: FeatureTriggers, target: "24.3", expectedStatus: StatusSupported},
		{name: "triggers after", tag: FeatureTriggers, target: "25.1", expectedStatus: StatusSupported},

		{name: "show ldr jobs before", tag: FeatureShowLogicalReplicationJobs, target: "24.2", expectedStatus: StatusNotYetIntroduced},
		{name: "show ldr jobs at", tag: FeatureShowLogicalReplicationJobs, target: "24.3", expectedStatus: StatusSupported},
		{name: "show ldr jobs after", tag: FeatureShowLogicalReplicationJobs, target: "25.1", expectedStatus: StatusSupported},

		{name: "ldr skip schema check before", tag: FeatureLDRSkipSchemaCheck, target: "24.2", expectedStatus: StatusNotYetIntroduced},
		{name: "ldr skip schema check at", tag: FeatureLDRSkipSchemaCheck, target: "24.3", expectedStatus: StatusSupported},
		{name: "ldr skip schema check after", tag: FeatureLDRSkipSchemaCheck, target: "25.1", expectedStatus: StatusSupported},

		{name: "vector index before", tag: FeatureVectorIndex, target: "24.3", expectedStatus: StatusNotYetIntroduced},
		{name: "vector index at", tag: FeatureVectorIndex, target: "25.1", expectedStatus: StatusSupported},
		{name: "vector index after", tag: FeatureVectorIndex, target: "25.2", expectedStatus: StatusSupported},

		{name: "rls before", tag: FeatureRowLevelSecurity, target: "24.3", expectedStatus: StatusNotYetIntroduced},
		{name: "rls at", tag: FeatureRowLevelSecurity, target: "25.1", expectedStatus: StatusSupported},
		{name: "rls after", tag: FeatureRowLevelSecurity, target: "25.2", expectedStatus: StatusSupported},

		{name: "check external conn before", tag: FeatureCheckExternalConnection, target: "24.3", expectedStatus: StatusNotYetIntroduced},
		{name: "check external conn at", tag: FeatureCheckExternalConnection, target: "25.1", expectedStatus: StatusSupported},
		{name: "check external conn after", tag: FeatureCheckExternalConnection, target: "25.2", expectedStatus: StatusSupported},

		{name: "ldr bidirectional before", tag: FeatureLDRBidirectional, target: "24.3", expectedStatus: StatusNotYetIntroduced},
		{name: "ldr bidirectional at", tag: FeatureLDRBidirectional, target: "25.1", expectedStatus: StatusSupported},
		{name: "ldr bidirectional after", tag: FeatureLDRBidirectional, target: "25.2", expectedStatus: StatusSupported},

		{name: "do block before", tag: FeatureDoBlock, target: "24.3", expectedStatus: StatusNotYetIntroduced},
		{name: "do block at", tag: FeatureDoBlock, target: "25.1", expectedStatus: StatusSupported},
		{name: "do block after", tag: FeatureDoBlock, target: "25.2", expectedStatus: StatusSupported},

		{name: "returns table before", tag: FeatureReturnsTable, target: "24.3", expectedStatus: StatusNotYetIntroduced},
		{name: "returns table at", tag: FeatureReturnsTable, target: "25.1", expectedStatus: StatusSupported},
		{name: "returns table after", tag: FeatureReturnsTable, target: "25.2", expectedStatus: StatusSupported},

		{name: "xa txn before", tag: FeatureXATransactions, target: "24.3", expectedStatus: StatusNotYetIntroduced},
		{name: "xa txn at", tag: FeatureXATransactions, target: "25.1", expectedStatus: StatusSupported},
		{name: "xa txn after", tag: FeatureXATransactions, target: "25.2", expectedStatus: StatusSupported},

		{name: "create policy if not exists before", tag: FeatureCreatePolicyIfNotExists, target: "25.1", expectedStatus: StatusNotYetIntroduced},
		{name: "create policy if not exists at", tag: FeatureCreatePolicyIfNotExists, target: "25.2", expectedStatus: StatusSupported},
		{name: "create policy if not exists after", tag: FeatureCreatePolicyIfNotExists, target: "25.3", expectedStatus: StatusSupported},

		{name: "refresh matview aost before", tag: FeatureRefreshMaterializedViewAsOf, target: "25.1", expectedStatus: StatusNotYetIntroduced},
		{name: "refresh matview aost at", tag: FeatureRefreshMaterializedViewAsOf, target: "25.2", expectedStatus: StatusSupported},
		{name: "refresh matview aost after", tag: FeatureRefreshMaterializedViewAsOf, target: "25.3", expectedStatus: StatusSupported},

		{name: "alter vc repl source before", tag: FeatureAlterVirtualClusterReplicationSrc, target: "25.1", expectedStatus: StatusNotYetIntroduced},
		{name: "alter vc repl source at", tag: FeatureAlterVirtualClusterReplicationSrc, target: "25.2", expectedStatus: StatusSupported},
		{name: "alter vc repl source after", tag: FeatureAlterVirtualClusterReplicationSrc, target: "25.3", expectedStatus: StatusSupported},

		{name: "show create all triggers before", tag: FeatureShowCreateAllTriggers, target: "25.2", expectedStatus: StatusNotYetIntroduced},
		{name: "show create all triggers at", tag: FeatureShowCreateAllTriggers, target: "25.3", expectedStatus: StatusSupported},
		{name: "show create all triggers after", tag: FeatureShowCreateAllTriggers, target: "25.4", expectedStatus: StatusSupported},

		{name: "show create all routines before", tag: FeatureShowCreateAllRoutines, target: "25.2", expectedStatus: StatusNotYetIntroduced},
		{name: "show create all routines at", tag: FeatureShowCreateAllRoutines, target: "25.3", expectedStatus: StatusSupported},
		{name: "show create all routines after", tag: FeatureShowCreateAllRoutines, target: "25.4", expectedStatus: StatusSupported},

		{name: "alter table logged toggle before", tag: FeatureAlterTableLoggedToggle, target: "25.2", expectedStatus: StatusNotYetIntroduced},
		{name: "alter table logged toggle at", tag: FeatureAlterTableLoggedToggle, target: "25.3", expectedStatus: StatusSupported},
		{name: "alter table logged toggle after", tag: FeatureAlterTableLoggedToggle, target: "25.4", expectedStatus: StatusSupported},

		{name: "grant revoke routines before", tag: FeatureGrantRevokeRoutines, target: "25.2", expectedStatus: StatusNotYetIntroduced},
		{name: "grant revoke routines at", tag: FeatureGrantRevokeRoutines, target: "25.3", expectedStatus: StatusSupported},
		{name: "grant revoke routines after", tag: FeatureGrantRevokeRoutines, target: "25.4", expectedStatus: StatusSupported},

		{name: "cross-major: target predates oldest seed", tag: FeaturePLpgSQLFunctionBody, target: "19.1", expectedStatus: StatusNotYetIntroduced},
		{name: "cross-major: target newer than newest seed", tag: FeaturePLpgSQLFunctionBody, target: "26.2", expectedStatus: StatusSupported},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotStatus, gotFeature := reg.Supports(tc.target, tc.tag)
			require.Equal(t, tc.expectedStatus, gotStatus)
			require.Equal(t, tc.tag, gotFeature.Tag,
				"Supports must return the matched feature so callers can build a warning without a second lookup")
		})
	}
}

func TestSupports_UnknownTag(t *testing.T) {
	reg := DefaultRegistry()
	status, feat := reg.Supports("25.4", "no_such_feature")
	require.Equal(t, StatusUnknown, status)
	require.Equal(t, Feature{}, feat)
}

func TestSupports_UnparseableTarget(t *testing.T) {
	reg := DefaultRegistry()
	status, _ := reg.Supports("not-a-version", FeaturePLpgSQLFunctionBody)
	require.Equal(t, StatusUnknown, status)
}

// TestSupports_MajorMinorPatchEquivalence pins the contract that
// "25.4" and "25.4.0" compare equal. A regression here would mean
// users who type a patch component get different warnings than
// users who type only MAJOR.MINOR.
func TestSupports_MajorMinorPatchEquivalence(t *testing.T) {
	reg := DefaultRegistry()
	a, _ := reg.Supports("24.1", FeaturePLpgSQLFunctionBody)
	b, _ := reg.Supports("24.1.0", FeaturePLpgSQLFunctionBody)
	c, _ := reg.Supports("24.1.99", FeaturePLpgSQLFunctionBody)
	require.Equal(t, a, b)
	require.Equal(t, b, c)
}

// TestStatusUnknown_IsZeroValue pins the load-bearing contract
// that Status's zero value is StatusUnknown. The registry treats
// "unknown" as the safe default; renumbering would silently turn
// uninitialized statuses into StatusSupported.
func TestStatusUnknown_IsZeroValue(t *testing.T) {
	var s Status
	require.Equal(t, StatusUnknown, s)
}

func TestLookup_Found(t *testing.T) {
	reg := DefaultRegistry()
	f, ok := reg.Lookup(FeaturePLpgSQLFunctionBody)
	require.True(t, ok)
	require.Equal(t, "24.1", f.Introduced)
}

func TestLookup_NotFound(t *testing.T) {
	reg := DefaultRegistry()
	_, ok := reg.Lookup("no_such_feature")
	require.False(t, ok)
}

// TestExportedConstantsAreRegistered enforces that every Feature*
// constant resolves to a registered feature in DefaultRegistry.
// Drift here is the one cost of maintaining both a constant list
// and a seed list; the test catches it rather than trusting
// reviewers.
func TestExportedConstantsAreRegistered(t *testing.T) {
	reg := DefaultRegistry()
	for _, tag := range []string{
		FeaturePLpgSQLFunctionBody,
		FeatureTrigramIndex,
		FeatureRegionalByRow,
		FeatureAlterChangefeed,
		FeatureTriggers,
		FeatureShowLogicalReplicationJobs,
		FeatureLDRSkipSchemaCheck,
		FeatureVectorIndex,
		FeatureRowLevelSecurity,
		FeatureCheckExternalConnection,
		FeatureLDRBidirectional,
		FeatureDoBlock,
		FeatureReturnsTable,
		FeatureXATransactions,
		FeatureCreatePolicyIfNotExists,
		FeatureRefreshMaterializedViewAsOf,
		FeatureAlterVirtualClusterReplicationSrc,
		FeatureShowCreateAllTriggers,
		FeatureShowCreateAllRoutines,
		FeatureAlterTableLoggedToggle,
		FeatureGrantRevokeRoutines,
	} {
		_, ok := reg.Lookup(tag)
		require.Truef(t, ok, "constant %q is not registered in DefaultRegistry", tag)
	}
}

// TestNewRegistry_PanicsOnMisconfiguration uses PanicsWithValue-style
// matching (substring on the panic message) so that each case
// asserts not just "panics" but "panics for the documented reason."
// Without this, a refactor that triggered a different panic could
// pass the test for the wrong cause.
func TestNewRegistry_PanicsOnMisconfiguration(t *testing.T) {
	tests := []struct {
		name                   string
		features               []Feature
		expectedPanicSubstring string
	}{
		{
			name:                   "empty tag",
			features:               []Feature{{Tag: "", Name: "x", Introduced: "24.1"}},
			expectedPanicSubstring: "empty Tag",
		},
		{
			name:                   "empty name",
			features:               []Feature{{Tag: "x", Name: "", Introduced: "24.1"}},
			expectedPanicSubstring: "empty Name",
		},
		{
			name: "duplicate tag",
			features: []Feature{
				{Tag: "x", Name: "x", Introduced: "24.1"},
				{Tag: "x", Name: "x", Introduced: "25.1"},
			},
			expectedPanicSubstring: "duplicate Tag",
		},
		{
			name:                   "invalid introduced",
			features:               []Feature{{Tag: "x", Name: "x", Introduced: "not-a-version"}},
			expectedPanicSubstring: "invalid Introduced",
		},
		{
			name:                   "invalid removed",
			features:               []Feature{{Tag: "x", Name: "x", Introduced: "24.1", Removed: "garbage"}},
			expectedPanicSubstring: "invalid Removed",
		},
		{
			name:                   "removed without introduced",
			features:               []Feature{{Tag: "x", Name: "x", Removed: "24.1"}},
			expectedPanicSubstring: "without Introduced",
		},
		{
			name:                   "removed equals introduced",
			features:               []Feature{{Tag: "x", Name: "x", Introduced: "24.1", Removed: "24.1"}},
			expectedPanicSubstring: "<= Introduced",
		},
		{
			name:                   "removed before introduced",
			features:               []Feature{{Tag: "x", Name: "x", Introduced: "25.1", Removed: "24.1"}},
			expectedPanicSubstring: "<= Introduced",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				r := recover()
				require.NotNil(t, r, "NewRegistry must panic")
				msg, ok := r.(string)
				require.Truef(t, ok, "panic value must be a string, got %T", r)
				require.Containsf(t, msg, tc.expectedPanicSubstring,
					"panic message %q must contain %q", msg, tc.expectedPanicSubstring)
			}()
			NewRegistry(tc.features...)
		})
	}
}

// TestRemovedStatus exercises the StatusRemoved branch using a
// hand-built registry, since no seeded feature has been removed.
// Without this test the StatusRemoved path would be dead code.
func TestRemovedStatus(t *testing.T) {
	reg := NewRegistry(Feature{
		Tag:        "deprecated_thing",
		Name:       "deprecated thing",
		Introduced: "20.1",
		Removed:    "24.1",
	})

	tests := []struct {
		name           string
		target         string
		expectedStatus Status
	}{
		{name: "before introduced", target: "19.2", expectedStatus: StatusNotYetIntroduced},
		{name: "at introduced", target: "20.1", expectedStatus: StatusSupported},
		{name: "between introduced and removed", target: "23.2", expectedStatus: StatusSupported},
		{name: "at removed", target: "24.1", expectedStatus: StatusRemoved},
		{name: "after removed", target: "25.4", expectedStatus: StatusRemoved},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := reg.Supports(tc.target, "deprecated_thing")
			require.Equal(t, tc.expectedStatus, got)
		})
	}
}

// TestEmptyIntroduced covers the documented contract that an empty
// Introduced string means "supported in every known version".
func TestEmptyIntroduced(t *testing.T) {
	reg := NewRegistry(Feature{Tag: "ancient", Name: "ancient feature"})
	got, _ := reg.Supports("1.0", "ancient")
	require.Equal(t, StatusSupported, got)
}
