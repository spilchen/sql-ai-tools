// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package semcheck

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser/statements"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/tree"

	"github.com/spilchen/sql-ai-tools/internal/diag"
	"github.com/spilchen/sql-ai-tools/internal/output"
)

// unknownFunctionCode is the SQLSTATE for "undefined_function" (PG
// 42883). The parser also surfaces this code from its own type-check
// path, but CheckFunctionNames runs first and CheckExprTypes skips
// FuncExprs with unresolved bare names so consumers see exactly one
// 42883 per typo — the structured one with suggestions and sampled
// available_functions context.
const unknownFunctionCode = "42883"

// availableFunctionsSampleSize caps the number of names returned in
// the Context["available_functions"] payload. The full builtin
// registry contains hundreds of names — dumping all of them inflates
// the wire payload without helping an agent narrow down a typo. The
// sample is the top-N nearest names by Levenshtein distance, which
// subsumes the suggestions[] cap (3) and gives the agent a few extra
// near-misses to consider.
const availableFunctionsSampleSize = 25

// builtinFuncNamesSnapshot returns the sorted slice of bare builtin
// function names registered in tree.FunDefs, computed exactly once per
// process. The registry is populated at init time by
// internal/builtinstubs and is immutable thereafter, so a single
// process-wide snapshot is correct and avoids re-walking ~10^3 names
// on every CheckFunctionNames call.
var builtinFuncNamesSnapshot = sync.OnceValue(builtinFunctionNames)

// CheckFunctionNames walks every statement in stmts and reports calls
// to function names that do not exist in the builtin registry
// (tree.FunDefs, populated by internal/builtinstubs at process start).
//
// Errors are deduplicated by lowercased function name across the whole
// input — a single missing name referenced from two statements
// produces one error, not two. The first reference's source position
// is reported.
//
// Schema-qualified calls (e.g. pg_catalog.foo()) are intentionally
// skipped: this check is name-existence only, and a meaningful
// schema-qualified lookup needs the per-search-path resolver that lives
// behind a catalog (out of scope for issue #107).
//
// fullSQL is the original SQL text used to compute 1-based line/column
// positions and to feed diag.Suggest's byte range. Returns nil when
// stmts is empty or every call resolves.
func CheckFunctionNames(stmts statements.Statements, fullSQL string) []output.Error {
	if len(stmts) == 0 {
		return nil
	}

	var errs []output.Error
	seen := make(map[string]struct{})

	stmtStart := 0
	for i := range stmts {
		stmt := stmts[i].AST
		if stmt == nil {
			continue
		}
		stmtSQL := stmts[i].SQL
		stmtEnd := stmtStart + len(stmtSQL)
		if rel := strings.Index(fullSQL[stmtStart:], stmtSQL); rel >= 0 {
			stmtStart += rel
			stmtEnd = stmtStart + len(stmtSQL)
		}

		v := &funcNameVisitor{
			fullSQL:   fullSQL,
			stmtStart: stmtStart,
			stmtEnd:   stmtEnd,
			seen:      seen,
			errs:      &errs,
		}
		tree.WalkStmt(v, stmt)

		stmtStart = stmtEnd
	}
	return errs
}

// funcNameVisitor walks one top-level statement collecting unresolved
// function-call references.
//
// Lifecycle: one visitor per statement; created by CheckFunctionNames
// and discarded once WalkStmt returns. seen is shared across statements
// so the same missing name only produces one error per input.
// stmtStart/stmtEnd bound the byte range searched by positionFor (the
// helper from names.go), keeping position lookups inside the current
// statement so a typo'd name in stmt 2 doesn't get located in stmt 1.
//
// The visitor implements tree.ExtendedVisitor — required because a
// plain tree.Visitor skips TableExpr subtrees during WalkStmt, but
// function calls can appear inside FROM (table-valued functions),
// JOIN ON conditions, and subqueries within FROM.
type funcNameVisitor struct {
	fullSQL   string
	stmtStart int
	stmtEnd   int
	seen      map[string]struct{}
	errs      *[]output.Error
}

var _ tree.ExtendedVisitor = (*funcNameVisitor)(nil)

func (v *funcNameVisitor) VisitPost(expr tree.Expr) tree.Expr                        { return expr }
func (v *funcNameVisitor) VisitTablePre(e tree.TableExpr) (bool, tree.TableExpr)     { return true, e }
func (v *funcNameVisitor) VisitTablePost(e tree.TableExpr) tree.TableExpr            { return e }
func (v *funcNameVisitor) VisitStatementPre(s tree.Statement) (bool, tree.Statement) { return true, s }
func (v *funcNameVisitor) VisitStatementPost(s tree.Statement) tree.Statement        { return s }

func (v *funcNameVisitor) VisitPre(expr tree.Expr) (bool, tree.Expr) {
	fn, ok := expr.(*tree.FuncExpr)
	if !ok {
		return true, expr
	}
	v.checkFunc(fn)
	return true, expr
}

// checkFunc reports fn's referenced name as unknown when the bare name
// is not in tree.FunDefs. Already-resolved function references (e.g.
// the grammar's WrapFunction calls for keywords like current_timestamp)
// and schema-qualified names are silently skipped; both are out of
// scope for the bare-name typo detection issue #107 targets.
func (v *funcNameVisitor) checkFunc(fn *tree.FuncExpr) {
	un, ok := fn.Func.FunctionReference.(*tree.UnresolvedName)
	if !ok {
		return
	}
	if un.NumParts != 1 {
		return
	}
	name := un.Parts[0]
	if name == "" {
		return
	}
	lower := strings.ToLower(name)
	if _, found := tree.FunDefs[lower]; found {
		return
	}
	if _, dup := v.seen[lower]; dup {
		return
	}
	v.seen[lower] = struct{}{}

	pos := v.positionFor(name)
	all := builtinFuncNamesSnapshot()
	*v.errs = append(*v.errs, output.Error{
		Code:     unknownFunctionCode,
		Severity: output.SeverityError,
		Message:  fmt.Sprintf("unknown function: %s()", name),
		Position: pos,
		Category: diag.CategoryUnknownFunction,
		Context: map[string]any{
			"available_functions": sampleNearest(name, all, availableFunctionsSampleSize),
		},
		Suggestions: diag.Suggest(name, all, pos),
	})
}

// positionFor mirrors tableNameVisitor.positionFor in names.go: it
// locates name within the current statement's byte range with word-
// boundary checks so a search for "now" doesn't match the "now" inside
// "snowflake". A separate copy here (rather than a shared free
// function) keeps the visitor's stmtStart/stmtEnd plumbing local; the
// underlying helpers (indexFold, isWordBoundary) live in names.go and
// are reused.
func (v *funcNameVisitor) positionFor(name string) *output.Position {
	if name == "" {
		return nil
	}
	stmtText := v.fullSQL[v.stmtStart:v.stmtEnd]
	for off := 0; off < len(stmtText); {
		rel := indexFold(stmtText[off:], name)
		if rel < 0 {
			return nil
		}
		abs := v.stmtStart + off + rel
		if isWordBoundary(v.fullSQL, abs, abs+len(name)) {
			return diag.PositionFromByteOffset(v.fullSQL, abs)
		}
		off += rel + 1
	}
	return nil
}

// builtinFunctionNames returns a sorted snapshot of every bare builtin
// function name registered in tree.FunDefs. Schema-qualified entries
// (e.g. "pg_catalog.length") are excluded so the suggestion candidate
// set matches the bare-name keys an unresolved UnresolvedName carries.
//
// Callers should invoke through builtinFuncNamesSnapshot, the
// package-level sync.OnceValue wrapper, so the work happens at most
// once per process. The function copies out of the global map under
// the assumption that internal/builtinstubs.Init has already run —
// registration is a one-shot init-time event, so the snapshot does
// not need to handle concurrent mutation.
func builtinFunctionNames() []string {
	names := make([]string, 0, len(tree.FunDefs))
	for name := range tree.FunDefs {
		if strings.Contains(name, ".") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// sampleNearest returns up to limit candidate names ranked by
// Levenshtein edit distance to misspelled. Unlike diag.Suggest it
// applies no distance threshold — the caller already gets a high-
// confidence subset via Suggestions; this sample is for the agent's
// fallback "show me what's nearby" use case, where even a moderately
// distant name (e.g. unrelated suffix variants) is useful context.
//
// Ties are broken alphabetically so the sample is deterministic across
// runs and across the suggestions[] subset (which uses the same
// secondary sort).
func sampleNearest(misspelled string, candidates []string, limit int) []string {
	if limit <= 0 || len(candidates) == 0 {
		return nil
	}
	type scored struct {
		name     string
		distance int
	}
	hits := make([]scored, 0, len(candidates))
	low := strings.ToLower(misspelled)
	for _, cand := range candidates {
		if cand == "" || strings.EqualFold(cand, misspelled) {
			continue
		}
		d := levenshtein(low, strings.ToLower(cand))
		hits = append(hits, scored{name: cand, distance: d})
	}
	if len(hits) == 0 {
		return nil
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].distance != hits[j].distance {
			return hits[i].distance < hits[j].distance
		}
		return hits[i].name < hits[j].name
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.name
	}
	return out
}

// levenshtein duplicates the two-row DP from diag.Suggest because
// that helper is unexported. The two implementations must stay
// behaviourally identical: sampleNearest's "available_functions"
// ranking and diag.Suggest's "did you mean?" ranking are presented
// side-by-side to the agent, and a divergence (e.g. one adopting
// transposition support or an early-exit optimization) would yield
// inconsistent orderings that look like a bug. If you change either
// copy, change the other — or promote the helper to a shared utility
// (e.g. an exported diag.Levenshtein) and delete one.
func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}
