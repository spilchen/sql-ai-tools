// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package version provides a registry of CockroachDB SQL features and
// the versions in which they were introduced (or removed).
//
// The registry is pure metadata. It does not inspect SQL or AST
// nodes; that is the job of the AST inspector layered on top of it
// (see issue #84). The contract between the two layers is the
// feature tag — a stable, machine-readable string that identifies a
// SQL feature independent of how the parser represents it.
//
// Tags are named for the user-facing feature ("regional_by_row"),
// not for the AST node ("create_table_locality_regional_by_row"),
// because a single feature may surface in multiple AST shapes
// (e.g. CREATE TABLE ... LOCALITY and ALTER TABLE ... SET LOCALITY).
// Naming by feature lets one registry entry cover every AST shape
// and produces warning messages a user will recognize.
//
// Lifecycle: a Registry is constructed once via NewRegistry (or
// DefaultRegistry for the seeded set) and is immutable afterward.
// Concurrent readers are safe; there is no mutation API.
package version

import (
	"fmt"
	"strconv"
	"strings"
)

// Feature describes a single CockroachDB SQL feature and the
// version range in which it is available.
//
// Introduced and Removed are MAJOR.MINOR strings (e.g. "24.1") in
// the same canonical form produced by output.ValidateTargetVersion.
// Patch components are not stored: feature gating is a
// minor-release-grained concern, and recording patch noise would
// invite false positives for users who specify "25.4.3".
//
//   - Introduced == "" means "supported in every known version"
//     (i.e. predates the registry's earliest entry of interest).
//   - Removed == "" means "still supported in current versions".
//
// DocURL is optional and intended for warning messages emitted by
// the AST inspector in #84.
type Feature struct {
	Tag        string
	Name       string
	Introduced string
	Removed    string
	DocURL     string
}

// Status reports whether a feature is available in a given target
// version. Returned by Registry.Supports.
type Status int

// Status values.
const (
	// StatusUnknown means the queried tag is not registered. Callers
	// that treat unknown tags as "do not warn" should check for this
	// value explicitly rather than relying on a boolean.
	//
	// StatusUnknown is intentionally the zero value, as a deliberate
	// exception to the project convention of starting enums at one.
	// The zero value is load-bearing here: a Status read from an
	// uninitialized variable, or returned for an unrecognized tag,
	// must be safe to interpret as "do not warn." Renumbering would
	// silently turn unknown lookups into StatusSupported.
	StatusUnknown Status = iota

	// StatusSupported means the target version is at or after the
	// feature's Introduced version, and before any Removed version.
	StatusSupported

	// StatusNotYetIntroduced means the target version predates the
	// feature's Introduced version. The AST inspector turns this
	// into a "feature requires vX.Y+" warning.
	StatusNotYetIntroduced

	// StatusRemoved means the target version is at or after the
	// feature's Removed version.
	StatusRemoved
)

// Feature tag constants. These are the stable identifiers the AST
// inspector (#84) emits when it recognizes a feature in parsed SQL.
// Adding a new tag means adding both a constant here and a matching
// Feature{Tag: ...} entry in DefaultRegistry; a test enforces that
// every exported constant is registered.
const (
	FeaturePLpgSQLFunctionBody = "plpgsql_function_body"
	FeatureTrigramIndex        = "trigram_index"
	FeatureRegionalByRow       = "regional_by_row"
	FeatureAlterChangefeed     = "alter_changefeed"

	// v24.3 features.
	FeatureTriggers                   = "triggers"
	FeatureShowLogicalReplicationJobs = "show_logical_replication_jobs"
	FeatureLDRSkipSchemaCheck         = "ldr_skip_schema_check"

	// v25.1 features.
	FeatureVectorIndex             = "vector_index"
	FeatureRowLevelSecurity        = "row_level_security"
	FeatureCheckExternalConnection = "check_external_connection"
	FeatureLDRBidirectional        = "ldr_bidirectional"
	FeatureDoBlock                 = "do_block"
	FeatureReturnsTable            = "returns_table"
	FeatureXATransactions          = "xa_transactions"

	// v25.2 features.
	FeatureCreatePolicyIfNotExists           = "create_policy_if_not_exists"
	FeatureRefreshMaterializedViewAsOf       = "refresh_materialized_view_as_of_system_time"
	FeatureAlterVirtualClusterReplicationSrc = "alter_virtual_cluster_replication_source"

	// v25.3 features.
	FeatureShowCreateAllTriggers  = "show_create_all_triggers"
	FeatureShowCreateAllRoutines  = "show_create_all_routines"
	FeatureAlterTableLoggedToggle = "alter_table_logged_unlogged"
	FeatureGrantRevokeRoutines    = "grant_revoke_routines"

	// v25.4 features.
	FeatureInspectCommand          = "inspect_command"
	FeatureShowInspectErrors       = "show_inspect_errors"
	FeatureLTreeType               = "ltree_type"
	FeatureChangefeedDatabaseLevel = "changefeed_database_level"
	FeatureAlterExternalConnection = "alter_external_connection"
)

// Registry holds an immutable set of Feature entries, indexed by
// tag for O(1) lookup. Build with NewRegistry or DefaultRegistry.
type Registry struct {
	byTag map[string]Feature
}

// NewRegistry constructs a Registry from the given features.
//
// It panics on misconfiguration so that registry mistakes surface at
// init time rather than as silent StatusUnknown results at query
// time. Specifically, it panics if any feature has an empty Tag, an
// empty Name, a duplicate Tag, an unparseable Introduced or Removed
// version, a non-empty Removed without a non-empty Introduced (the
// "removed but never introduced" shape is undefined), or a Removed
// version that is not strictly greater than Introduced.
func NewRegistry(features ...Feature) *Registry {
	byTag := make(map[string]Feature, len(features))
	for _, f := range features {
		if f.Tag == "" {
			panic("version: feature has empty Tag")
		}
		if f.Name == "" {
			panic(fmt.Sprintf("version: feature %q has empty Name", f.Tag))
		}
		if _, dup := byTag[f.Tag]; dup {
			panic(fmt.Sprintf("version: duplicate Tag %q", f.Tag))
		}
		if f.Introduced != "" {
			if _, ok := parseMajorMinor(f.Introduced); !ok {
				panic(fmt.Sprintf("version: feature %q has invalid Introduced %q", f.Tag, f.Introduced))
			}
		}
		if f.Removed != "" {
			if f.Introduced == "" {
				panic(fmt.Sprintf("version: feature %q has Removed %q without Introduced", f.Tag, f.Removed))
			}
			if _, ok := parseMajorMinor(f.Removed); !ok {
				panic(fmt.Sprintf("version: feature %q has invalid Removed %q", f.Tag, f.Removed))
			}
			if compareMajorMinor(f.Removed, f.Introduced) <= 0 {
				panic(fmt.Sprintf("version: feature %q has Removed %q <= Introduced %q",
					f.Tag, f.Removed, f.Introduced))
			}
		}
		byTag[f.Tag] = f
	}
	return &Registry{byTag: byTag}
}

// DefaultRegistry returns the seeded set of CockroachDB feature
// entries. The set is intentionally small; it will grow over time as
// the AST inspector learns to recognize more features.
func DefaultRegistry() *Registry {
	return NewRegistry(
		Feature{
			Tag:        FeaturePLpgSQLFunctionBody,
			Name:       "PL/pgSQL function bodies",
			Introduced: "24.1",
		},
		Feature{
			Tag:        FeatureTrigramIndex,
			Name:       "trigram indexes",
			Introduced: "23.1",
		},
		Feature{
			Tag:        FeatureRegionalByRow,
			Name:       "REGIONAL BY ROW tables",
			Introduced: "21.1",
		},
		Feature{
			Tag:        FeatureAlterChangefeed,
			Name:       "ALTER CHANGEFEED",
			Introduced: "22.1",
		},

		// v24.3 features.
		Feature{
			Tag:        FeatureTriggers,
			Name:       "triggers (CREATE/ALTER/DROP TRIGGER)",
			Introduced: "24.3",
		},
		Feature{
			Tag:        FeatureShowLogicalReplicationJobs,
			Name:       "SHOW LOGICAL REPLICATION JOBS",
			Introduced: "24.3",
		},
		Feature{
			Tag:        FeatureLDRSkipSchemaCheck,
			Name:       "LDR SKIP SCHEMA CHECK option",
			Introduced: "24.3",
		},

		// v25.1 features.
		Feature{
			Tag:        FeatureVectorIndex,
			Name:       "vector indexes (CSPANN)",
			Introduced: "25.1",
		},
		Feature{
			Tag:        FeatureRowLevelSecurity,
			Name:       "row-level security (CREATE/ALTER/DROP POLICY)",
			Introduced: "25.1",
		},
		Feature{
			Tag:        FeatureCheckExternalConnection,
			Name:       "CHECK EXTERNAL CONNECTION",
			Introduced: "25.1",
		},
		Feature{
			Tag:        FeatureLDRBidirectional,
			Name:       "LDR BIDIRECTIONAL option",
			Introduced: "25.1",
		},
		Feature{
			Tag:        FeatureDoBlock,
			Name:       "DO (anonymous PL/pgSQL block)",
			Introduced: "25.1",
		},
		Feature{
			Tag:        FeatureReturnsTable,
			Name:       "RETURNS TABLE for user-defined functions",
			Introduced: "25.1",
		},
		Feature{
			Tag:        FeatureXATransactions,
			Name:       "XA two-phase commit (PREPARE/COMMIT/ROLLBACK PREPARED)",
			Introduced: "25.1",
		},

		// v25.2 features.
		Feature{
			Tag:        FeatureCreatePolicyIfNotExists,
			Name:       "CREATE POLICY IF NOT EXISTS",
			Introduced: "25.2",
		},
		Feature{
			Tag:        FeatureRefreshMaterializedViewAsOf,
			Name:       "REFRESH MATERIALIZED VIEW ... AS OF SYSTEM TIME",
			Introduced: "25.2",
		},
		Feature{
			Tag:        FeatureAlterVirtualClusterReplicationSrc,
			Name:       "ALTER VIRTUAL CLUSTER ... SET REPLICATION SOURCE",
			Introduced: "25.2",
		},

		// v25.3 features.
		Feature{
			Tag:        FeatureShowCreateAllTriggers,
			Name:       "SHOW CREATE ALL TRIGGERS",
			Introduced: "25.3",
		},
		Feature{
			Tag:        FeatureShowCreateAllRoutines,
			Name:       "SHOW CREATE ALL ROUTINES",
			Introduced: "25.3",
		},
		Feature{
			Tag:        FeatureAlterTableLoggedToggle,
			Name:       "ALTER TABLE ... SET LOGGED/UNLOGGED",
			Introduced: "25.3",
		},
		Feature{
			Tag:        FeatureGrantRevokeRoutines,
			Name:       "GRANT/REVOKE ON ALL ROUTINES IN SCHEMA",
			Introduced: "25.3",
		},

		// v25.4 features.
		Feature{
			Tag:        FeatureInspectCommand,
			Name:       "INSPECT (table/database consistency check)",
			Introduced: "25.4",
		},
		Feature{
			Tag:        FeatureShowInspectErrors,
			Name:       "SHOW INSPECT ERRORS",
			Introduced: "25.4",
		},
		Feature{
			Tag:        FeatureLTreeType,
			Name:       "LTREE type",
			Introduced: "25.4",
		},
		Feature{
			Tag:        FeatureChangefeedDatabaseLevel,
			Name:       "CREATE DATABASE CHANGEFEED",
			Introduced: "25.4",
		},
		Feature{
			Tag:        FeatureAlterExternalConnection,
			Name:       "ALTER EXTERNAL CONNECTION",
			Introduced: "25.4",
		},
	)
}

// Lookup returns the Feature registered under tag, if any. The bool
// is false when the tag is not registered.
func (r *Registry) Lookup(tag string) (Feature, bool) {
	f, ok := r.byTag[tag]
	return f, ok
}

// Supports reports whether the feature identified by tag is
// available in target. target is expected to be in canonical form
// (post-output.ValidateTargetVersion): MAJOR.MINOR or
// MAJOR.MINOR.PATCH, no leading "v". Comparison ignores the patch
// component — feature gating is a minor-release concern.
//
// The returned Feature is the registered entry (zero value when
// status is StatusUnknown), so callers can build a warning message
// without a separate Lookup call.
//
// If target itself is unparseable, Supports returns
// (StatusUnknown, zero Feature). Callers should validate target
// once at the boundary rather than relying on this fallback.
func (r *Registry) Supports(target, tag string) (Status, Feature) {
	f, ok := r.byTag[tag]
	if !ok {
		return StatusUnknown, Feature{}
	}
	if _, ok := parseMajorMinor(target); !ok {
		return StatusUnknown, Feature{}
	}
	if f.Introduced != "" && compareMajorMinor(target, f.Introduced) < 0 {
		return StatusNotYetIntroduced, f
	}
	if f.Removed != "" && compareMajorMinor(target, f.Removed) >= 0 {
		return StatusRemoved, f
	}
	return StatusSupported, f
}

// parseMajorMinor extracts (major, minor) from a version string,
// tolerating a leading "v" and any number of additional
// dot-separated suffix components (patch, build metadata).
// Returns (_, false) when the string does not start with two
// dot-separated unsigned-numeric components. Mirrors the contract
// of output.majorMinor but returns the parsed numbers so callers
// can compare without a second string-split.
func parseMajorMinor(v string) ([2]uint64, bool) {
	v = strings.TrimPrefix(v, "v")
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return [2]uint64{}, false
	}
	major, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return [2]uint64{}, false
	}
	minor, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return [2]uint64{}, false
	}
	return [2]uint64{major, minor}, true
}

// compareMajorMinor returns -1, 0, or +1 when a is less than, equal
// to, or greater than b at MAJOR.MINOR resolution. Panics if either
// input is unparseable. The precondition is enforced rather than
// tolerated because every in-package caller validates upstream
// (NewRegistry validates Introduced/Removed, Supports validates
// target), so reaching this function with bad input indicates a
// programmer error that should fail loudly.
func compareMajorMinor(a, b string) int {
	av, ok := parseMajorMinor(a)
	if !ok {
		panic(fmt.Sprintf("version: compareMajorMinor: unparseable %q", a))
	}
	bv, ok := parseMajorMinor(b)
	if !ok {
		panic(fmt.Sprintf("version: compareMajorMinor: unparseable %q", b))
	}
	switch {
	case av[0] != bv[0]:
		if av[0] < bv[0] {
			return -1
		}
		return 1
	case av[1] != bv[1]:
		if av[1] < bv[1] {
			return -1
		}
		return 1
	}
	return 0
}
