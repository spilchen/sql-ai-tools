// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package version

import (
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser/statements"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/tree"

	"github.com/spilchen/sql-ai-tools/internal/output"
)

// defaultRegistry is the package-level Registry singleton consumed by
// Inspect when its reg argument is nil. Built once at init time so
// per-request callers do not pay the cost of constructing (and
// validating) the seeded set on every parse/validate invocation. Must
// stay equivalent to DefaultRegistry().
var defaultRegistry = DefaultRegistry()

// Inspect walks each top-level statement in stmts, detects any
// CockroachDB SQL features it recognizes, and returns one
// output.Error (Severity=WARNING, Code=CodeFeatureNotYetIntroduced)
// per detected feature whose Introduced version (per reg) is newer
// than target. Warnings are advisory: the parse / validate verdict
// is unchanged.
//
// target must be in canonical form (post-output.ValidateTargetVersion):
// MAJOR.MINOR or MAJOR.MINOR.PATCH, no leading "v". An empty target
// returns nil so callers can invoke Inspect unconditionally without
// guarding on whether --target-version was supplied.
//
// reg may be nil, in which case the package-level defaultRegistry
// (seeded from DefaultRegistry) is used. Tests pass an explicit
// registry to exercise hand-rolled features.
//
// Each returned Error carries Context entries — feature_tag,
// feature_name, introduced, target — so agents can branch
// programmatically without parsing the human-readable Message.
//
// Precondition: target has already been validated by the caller (the
// CLI does this in cmd/root via output.ValidateTargetVersion; the MCP
// surface does it in resolveTargetVersion). An unparseable target
// silently returns no warnings, mirroring Registry.Supports.
func Inspect(stmts statements.Statements, target string, reg *Registry) []output.Error {
	if target == "" || len(stmts) == 0 {
		return nil
	}
	if reg == nil {
		reg = defaultRegistry
	}

	var warnings []output.Error
	for _, stmt := range stmts {
		// Per-statement dedupe: a CREATE TABLE ... LOCALITY REGIONAL
		// BY ROW with a trigram index would otherwise emit two
		// warnings for the same locality if the visitor surfaced it
		// twice. The seen set scopes to one statement so a multi-
		// statement input still warns once per statement.
		seen := make(map[string]struct{})
		for _, tag := range detectFeatures(stmt.AST) {
			if _, dup := seen[tag]; dup {
				continue
			}
			seen[tag] = struct{}{}
			status, feat := reg.Supports(target, tag)
			if status != StatusNotYetIntroduced {
				continue
			}
			warnings = append(warnings, buildWarning(feat, target))
		}
	}
	return warnings
}

// detectFeatures returns the set of registry tags whose feature is
// referenced by stmt. The detector deliberately uses a top-level
// type switch rather than a tree.WalkStmt visitor: each seeded
// feature lives in a specific statement shape (CreateRoutine,
// CreateTable, AlterTable, AlterTableLocality, CreateIndex,
// AlterChangefeed), and a deep walk would surface no additional
// cases while complicating IndexElem traversal (IndexElem is not
// visited by tree.WalkStmt's Expr/Statement/TableExpr method set).
//
// The current implementation cannot return a duplicate tag for one
// statement, but Inspect dedupes anyway so future detectors that
// surface the same tag from multiple subtrees stay safe.
func detectFeatures(stmt tree.Statement) []string {
	var tags []string
	switch s := stmt.(type) {
	case *tree.CreateRoutine:
		if hasPLpgSQLBody(s) {
			tags = append(tags, FeaturePLpgSQLFunctionBody)
		}
	case *tree.CreateTable:
		if isRegionalByRow(s.Locality) {
			tags = append(tags, FeatureRegionalByRow)
		}
		if defsHaveTrigramIndex(s.Defs) {
			tags = append(tags, FeatureTrigramIndex)
		}
	case *tree.AlterTableLocality:
		// The parser surfaces ALTER TABLE ... SET LOCALITY ... as a
		// distinct top-level statement, not an AlterTable with a
		// locality command, so this case stands alone rather than
		// nesting inside *tree.AlterTable.
		if isRegionalByRow(s.Locality) {
			tags = append(tags, FeatureRegionalByRow)
		}
	case *tree.AlterTable:
		// ALTER TABLE ... ADD CONSTRAINT ... UNIQUE (col gin_trgm_ops)
		// is the realistic shape that lets a user retrofit a trigram
		// index without a full CREATE INDEX. The constraint def reuses
		// IndexElemList, so the same opclass check applies. We walk
		// every cmd rather than short-circuit so the dedupe contract
		// in Inspect handles multi-cmd statements (e.g. two ADD
		// CONSTRAINT clauses in one ALTER TABLE) uniformly with the
		// other detector arms.
		for _, cmd := range s.Cmds {
			add, ok := cmd.(*tree.AlterTableAddConstraint)
			if !ok {
				continue
			}
			uc, ok := add.ConstraintDef.(*tree.UniqueConstraintTableDef)
			if !ok {
				continue
			}
			if elemsHaveTrigramOpClass(uc.Columns) {
				tags = append(tags, FeatureTrigramIndex)
			}
		}
	case *tree.CreateIndex:
		if elemsHaveTrigramOpClass(s.Columns) {
			tags = append(tags, FeatureTrigramIndex)
		}
	case *tree.AlterChangefeed:
		tags = append(tags, FeatureAlterChangefeed)
	}
	return tags
}

// hasPLpgSQLBody reports whether the routine declares LANGUAGE
// plpgsql. The check mirrors upstream cockroachdb-parser's own
// detection in create_routine.go: scan the RoutineOption list for a
// RoutineLanguage value of RoutineLangPLpgSQL. The parser normalizes
// "PLpgSQL", "plpgsql", "PLPGSQL" to the same RoutineLangPLpgSQL
// value, so a case-fold compare is unnecessary.
func hasPLpgSQLBody(s *tree.CreateRoutine) bool {
	for _, opt := range s.Options {
		if lang, ok := opt.(tree.RoutineLanguage); ok && lang == tree.RoutineLangPLpgSQL {
			return true
		}
	}
	return false
}

// isRegionalByRow reports whether loc declares REGIONAL BY ROW
// placement. nil is treated as "no locality declared," not as a
// match — REGIONAL BY ROW must be explicit in the SQL to be flagged.
func isRegionalByRow(loc *tree.Locality) bool {
	return loc != nil && loc.LocalityLevel == tree.LocalityLevelRow
}

// defsHaveTrigramIndex scans the table definitions for an inline
// INDEX clause that uses a trigram opclass on any of its columns.
// Inline indexes parse into *tree.IndexTableDef (and
// *tree.UniqueConstraintTableDef.IndexTableDef for unique forms);
// both share the same IndexElemList shape so the inner check is
// reused.
func defsHaveTrigramIndex(defs tree.TableDefs) bool {
	for _, def := range defs {
		switch d := def.(type) {
		case *tree.IndexTableDef:
			if elemsHaveTrigramOpClass(d.Columns) {
				return true
			}
		case *tree.UniqueConstraintTableDef:
			if elemsHaveTrigramOpClass(d.Columns) {
				return true
			}
		}
	}
	return false
}

// elemsHaveTrigramOpClass reports whether any element in elems uses
// a trigram operator class (gin_trgm_ops or gist_trgm_ops). The
// match is case-insensitive because tree.Name preserves the user's
// casing — a future regression that downcased OpClass at parse time
// would otherwise silently break detection here.
func elemsHaveTrigramOpClass(elems tree.IndexElemList) bool {
	for _, e := range elems {
		oc := strings.ToLower(string(e.OpClass))
		if oc == "gin_trgm_ops" || oc == "gist_trgm_ops" {
			return true
		}
	}
	return false
}

// buildWarning constructs the user-facing Error for a detected feature
// whose Introduced version is newer than target. The Message follows
// the demo wording in issue #84; the Context map carries the same
// data in machine-readable form so agents do not need to re-parse the
// string. DocURL is included only when non-empty so the JSON payload
// stays tight for the common (no-docs-link) case.
func buildWarning(feat Feature, target string) output.Error {
	ctx := map[string]any{
		"feature_tag":  feat.Tag,
		"feature_name": feat.Name,
		"introduced":   feat.Introduced,
		"target":       target,
	}
	if feat.DocURL != "" {
		ctx["doc_url"] = feat.DocURL
	}
	return output.Error{
		Code:     output.CodeFeatureNotYetIntroduced,
		Severity: output.SeverityWarning,
		// Phrased "X required for Y" rather than "Y requires X" so the
		// sentence reads naturally for both singular feature names
		// ("ALTER CHANGEFEED") and plural ones ("PL/pgSQL function
		// bodies") — the latter would force a "bodies requires"
		// disagreement under the inverse phrasing.
		Message: fmt.Sprintf(
			"CockroachDB v%s+ required for %s; target version is %s",
			feat.Introduced, feat.Name, target,
		),
		Context: ctx,
	}
}
