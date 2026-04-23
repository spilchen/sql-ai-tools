// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package version

import (
	"testing"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/output"
)

// TestInspect_FeatureBoundaries pins one (feature, target) per
// detector at "before introduced" (warns) and "at introduced" (no
// warning). The feature-name and code shape are checked once via
// findWarningByTag below; this table keeps the per-feature surface
// small and readable.
func TestInspect_FeatureBoundaries(t *testing.T) {
	plpgsqlSQL := `CREATE FUNCTION f() RETURNS INT LANGUAGE PLpgSQL AS $$ BEGIN RETURN 1; END $$`
	rbrCreateSQL := `CREATE TABLE t (id INT PRIMARY KEY) LOCALITY REGIONAL BY ROW`
	rbrAlterSQL := `ALTER TABLE t SET LOCALITY REGIONAL BY ROW`
	trigramCreateIndexSQL := `CREATE INDEX i ON t USING GIN (col gin_trgm_ops)`
	trigramInlineIndexSQL := `CREATE TABLE t (col TEXT, INVERTED INDEX (col gin_trgm_ops))`
	alterChangefeedSQL := `ALTER CHANGEFEED 12345 SET resolved = '5s'`

	tests := []struct {
		name        string
		sql         string
		target      string
		expectedTag string // empty means: no warning expected
	}{
		{name: "plpgsql before introduced", sql: plpgsqlSQL, target: "23.2", expectedTag: FeaturePLpgSQLFunctionBody},
		{name: "plpgsql at introduced", sql: plpgsqlSQL, target: "24.1", expectedTag: ""},
		{name: "plpgsql after introduced", sql: plpgsqlSQL, target: "25.4", expectedTag: ""},

		{name: "regional by row create before", sql: rbrCreateSQL, target: "20.2", expectedTag: FeatureRegionalByRow},
		{name: "regional by row create at", sql: rbrCreateSQL, target: "21.1", expectedTag: ""},
		{name: "regional by row alter before", sql: rbrAlterSQL, target: "20.2", expectedTag: FeatureRegionalByRow},
		{name: "regional by row alter at", sql: rbrAlterSQL, target: "21.1", expectedTag: ""},

		{name: "trigram create index before", sql: trigramCreateIndexSQL, target: "22.2", expectedTag: FeatureTrigramIndex},
		{name: "trigram create index at", sql: trigramCreateIndexSQL, target: "23.1", expectedTag: ""},
		{name: "trigram inline index before", sql: trigramInlineIndexSQL, target: "22.2", expectedTag: FeatureTrigramIndex},
		{name: "trigram inline index at", sql: trigramInlineIndexSQL, target: "23.1", expectedTag: ""},

		{name: "alter changefeed before", sql: alterChangefeedSQL, target: "21.2", expectedTag: FeatureAlterChangefeed},
		{name: "alter changefeed at", sql: alterChangefeedSQL, target: "22.1", expectedTag: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stmts, err := parser.Parse(tc.sql)
			require.NoError(t, err)
			warnings := Inspect(stmts, tc.target, nil)

			if tc.expectedTag == "" {
				require.Empty(t, warnings, "expected no warnings, got %+v", warnings)
				return
			}

			w := findWarningByTag(warnings, tc.expectedTag)
			require.NotNilf(t, w, "expected warning with feature_tag=%q in %+v", tc.expectedTag, warnings)
			require.Equal(t, output.CodeFeatureNotYetIntroduced, w.Code)
			require.Equal(t, output.SeverityWarning, w.Severity)
			require.Equal(t, tc.target, w.Context["target"])
			require.NotEmpty(t, w.Context["feature_name"])
			require.NotEmpty(t, w.Context["introduced"])
			require.Contains(t, w.Message, w.Context["feature_name"].(string))
		})
	}
}

// TestInspect_EmptyTargetSkips pins the documented short-circuit:
// callers can invoke Inspect unconditionally; an empty target means
// "no --target-version was supplied" and produces no warnings.
func TestInspect_EmptyTargetSkips(t *testing.T) {
	stmts, err := parser.Parse(`CREATE FUNCTION f() RETURNS INT LANGUAGE PLpgSQL AS $$ BEGIN RETURN 1; END $$`)
	require.NoError(t, err)
	require.Empty(t, Inspect(stmts, "", nil))
}

// TestInspect_EmptyStmtsSkips pins the empty-stmts short-circuit so
// callers can pass a nil/empty slice (e.g. the empty-input MCP path)
// without producing spurious warnings.
func TestInspect_EmptyStmtsSkips(t *testing.T) {
	require.Empty(t, Inspect(nil, "23.2", nil))
}

// TestInspect_UnknownFeatureNoWarning makes sure that a statement type
// the inspector doesn't recognize (here: a plain SELECT) produces no
// warnings even at an ancient target version. Without this, a future
// refactor that accidentally promoted "no detector" to "warn anyway"
// would slip through.
func TestInspect_UnknownFeatureNoWarning(t *testing.T) {
	stmts, err := parser.Parse(`SELECT 1`)
	require.NoError(t, err)
	require.Empty(t, Inspect(stmts, "1.0", nil))
}

// TestInspect_PLpgSQLDetectorNegativeCases pins that the routine
// detector fires only on LANGUAGE plpgsql. A regression that dropped
// the language check (or inverted it) would warn on every CREATE
// FUNCTION and the existing positive cases would still pass.
func TestInspect_PLpgSQLDetectorNegativeCases(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{name: "LANGUAGE SQL must not warn", sql: `CREATE FUNCTION f() RETURNS INT LANGUAGE SQL AS 'SELECT 1'`},
		{name: "no LANGUAGE clause must not warn", sql: `CREATE FUNCTION f() RETURNS INT AS 'SELECT 1' LANGUAGE SQL`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stmts, err := parser.Parse(tc.sql)
			require.NoError(t, err)
			require.Empty(t, Inspect(stmts, "1.0", nil))
		})
	}
}

// TestInspect_TrigramOpClassVariants exercises the gist_trgm_ops
// branch and the case-fold path of elemsHaveTrigramOpClass — both
// of which are unreached by the boundary table above (which only
// uses lowercase gin_trgm_ops).
func TestInspect_TrigramOpClassVariants(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{name: "gist_trgm_ops on CREATE INDEX", sql: `CREATE INDEX i ON t USING GIST (col gist_trgm_ops)`},
		{name: "uppercase GIN_TRGM_OPS", sql: `CREATE INDEX i ON t USING GIN (col GIN_TRGM_OPS)`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stmts, err := parser.Parse(tc.sql)
			require.NoError(t, err)
			warnings := Inspect(stmts, "22.2", nil)
			require.Len(t, warnings, 1)
			require.Equal(t, FeatureTrigramIndex, warnings[0].Context["feature_tag"])
		})
	}
}

// TestInspect_TrigramViaAlterTableAddConstraint covers the realistic
// retrofit pattern: ALTER TABLE ... ADD CONSTRAINT u UNIQUE (col
// gin_trgm_ops). Without the *tree.AlterTable arm in detectFeatures
// this case silently bypasses the warning.
func TestInspect_TrigramViaAlterTableAddConstraint(t *testing.T) {
	stmts, err := parser.Parse(`ALTER TABLE t ADD CONSTRAINT u UNIQUE (col gin_trgm_ops)`)
	require.NoError(t, err)
	warnings := Inspect(stmts, "22.2", nil)
	require.Len(t, warnings, 1)
	require.Equal(t, FeatureTrigramIndex, warnings[0].Context["feature_tag"])
}

// TestInspect_AlterTableNegativeCases exercises every short-circuit
// branch in the *tree.AlterTable arm: a non-AddConstraint cmd, an
// AddConstraint with a non-Unique constraint def, and a UNIQUE
// constraint without a trigram opclass. All three must produce no
// warning. Without these, a refactor that flipped any inner type
// assertion would still pass the positive
// TestInspect_TrigramViaAlterTableAddConstraint above.
func TestInspect_AlterTableNegativeCases(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{name: "ADD COLUMN: non-AddConstraint cmd", sql: `ALTER TABLE t ADD COLUMN x INT`},
		{name: "ADD CONSTRAINT CHECK: non-Unique constraint", sql: `ALTER TABLE t ADD CONSTRAINT c CHECK (x > 0)`},
		{name: "ADD CONSTRAINT UNIQUE without opclass", sql: `ALTER TABLE t ADD CONSTRAINT u UNIQUE (col)`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stmts, err := parser.Parse(tc.sql)
			require.NoError(t, err)
			require.Empty(t, Inspect(stmts, "22.2", nil))
		})
	}
}

// TestInspect_AlterTableMultiCmdDedupes pins that two trigram-bearing
// ADD CONSTRAINT clauses in one ALTER TABLE produce exactly one
// warning. The arm walks every cmd (no short-circuit), and the
// per-statement dedupe in Inspect collapses repeats of the same tag.
// Without this test, dropping the dedupe would silently double-warn.
func TestInspect_AlterTableMultiCmdDedupes(t *testing.T) {
	stmts, err := parser.Parse(
		`ALTER TABLE t ADD CONSTRAINT u1 UNIQUE (a gin_trgm_ops), ADD CONSTRAINT u2 UNIQUE (b gin_trgm_ops)`)
	require.NoError(t, err)
	warnings := Inspect(stmts, "22.2", nil)
	require.Len(t, warnings, 1)
	require.Equal(t, FeatureTrigramIndex, warnings[0].Context["feature_tag"])
}

// TestInspect_RemovedFeatureNoWarning is the dead-code guard for the
// "status != StatusNotYetIntroduced" branch in Inspect. No seeded
// feature has Removed set today, so without an explicit registry a
// refactor that flipped the comparison to "== StatusSupported" would
// start spamming warnings for removed features and no test would
// catch it.
func TestInspect_RemovedFeatureNoWarning(t *testing.T) {
	custom := NewRegistry(Feature{
		Tag:        FeaturePLpgSQLFunctionBody,
		Name:       "deprecated plpgsql",
		Introduced: "20.1",
		Removed:    "24.1",
	})
	stmts, err := parser.Parse(`CREATE FUNCTION f() RETURNS INT LANGUAGE PLpgSQL AS $$ BEGIN RETURN 1; END $$`)
	require.NoError(t, err)
	require.Empty(t, Inspect(stmts, "25.4", custom),
		"target past Removed must not produce a NotYetIntroduced warning")
}

// TestInspect_MultiStatementWarnsPerStatement covers the contract that
// each statement is inspected independently: a script mixing one
// flagged feature and one supported statement produces exactly one
// warning, attributed by tag (we don't carry per-statement positions
// in the warning today, but the count must match).
func TestInspect_MultiStatementWarnsPerStatement(t *testing.T) {
	sql := `SELECT 1;
CREATE FUNCTION f() RETURNS INT LANGUAGE PLpgSQL AS $$ BEGIN RETURN 1; END $$;
CREATE TABLE t (id INT PRIMARY KEY) LOCALITY REGIONAL BY ROW`
	stmts, err := parser.Parse(sql)
	require.NoError(t, err)

	warnings := Inspect(stmts, "20.2", nil)
	require.Len(t, warnings, 2)

	require.NotNil(t, findWarningByTag(warnings, FeaturePLpgSQLFunctionBody))
	require.NotNil(t, findWarningByTag(warnings, FeatureRegionalByRow))
}

// TestInspect_DedupesWithinStatement asserts the per-statement seen
// set: a single CREATE TABLE that surfaces the same tag through two
// detection paths (e.g. inline INDEX trigram check seen twice if the
// def list is malformed) emits exactly one warning per tag.
func TestInspect_DedupesWithinStatement(t *testing.T) {
	// A CREATE TABLE with two trigram inline indexes on different
	// columns must still warn only once for the trigram tag.
	sql := `CREATE TABLE t (
		a TEXT,
		b TEXT,
		INVERTED INDEX (a gin_trgm_ops),
		INVERTED INDEX (b gin_trgm_ops)
	)`
	stmts, err := parser.Parse(sql)
	require.NoError(t, err)

	warnings := Inspect(stmts, "22.2", nil)
	require.Len(t, warnings, 1)
	require.Equal(t, FeatureTrigramIndex, warnings[0].Context["feature_tag"])
}

// TestInspect_ExplicitRegistry pins that a caller-supplied registry
// overrides the package singleton — required for tests of as-yet-
// unseeded features and for hypothetical downstream callers who want
// to gate on a custom feature set.
func TestInspect_ExplicitRegistry(t *testing.T) {
	custom := NewRegistry(Feature{
		Tag:        FeaturePLpgSQLFunctionBody,
		Name:       "custom plpgsql label",
		Introduced: "99.9",
	})
	stmts, err := parser.Parse(`CREATE FUNCTION f() RETURNS INT LANGUAGE PLpgSQL AS $$ BEGIN RETURN 1; END $$`)
	require.NoError(t, err)

	warnings := Inspect(stmts, "25.4", custom)
	require.Len(t, warnings, 1)
	require.Equal(t, "custom plpgsql label", warnings[0].Context["feature_name"])
}

// TestInspect_DefaultRegistrySingletonMatchesDefault pins the
// load-bearing contract that the package-level defaultRegistry is the
// same content as DefaultRegistry(). A refactor that drifted them
// apart would silently change which warnings the production CLI and
// MCP surfaces emit, with no test coverage at the call sites.
func TestInspect_DefaultRegistrySingletonMatchesDefault(t *testing.T) {
	require.Equal(t, DefaultRegistry().byTag, defaultRegistry.byTag)
}

// findWarningByTag returns the first warning whose Context.feature_tag
// matches tag, or nil. Tests use it instead of indexing into the
// slice so a future detector that adds a second warning per statement
// (e.g. a side-effect tag) doesn't break unrelated assertions.
func findWarningByTag(warnings []output.Error, tag string) *output.Error {
	for i := range warnings {
		if got, _ := warnings[i].Context["feature_tag"].(string); got == tag {
			return &warnings[i]
		}
	}
	return nil
}
