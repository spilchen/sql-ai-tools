// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package builtinstubs

import (
	"testing"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/tree"
	"github.com/stretchr/testify/require"
)

func init() {
	Init("")
}

func TestDefaultVersionRegistered(t *testing.T) {
	require.Contains(t, versionRegistry, DefaultVersion)
}

func TestActiveVersion(t *testing.T) {
	require.Equal(t, DefaultVersion, ActiveVersion())
}

func TestSupportedVersions(t *testing.T) {
	vs := SupportedVersions()
	require.NotEmpty(t, vs)
	require.Contains(t, vs, DefaultVersion)
}

func TestFunDefsPopulated(t *testing.T) {
	require.NotEmpty(t, tree.FunDefs)
	require.Greater(t, len(tree.FunDefs), 200)
}

func TestWellKnownFunctionsRegistered(t *testing.T) {
	for _, name := range []string{
		"length", "upper", "lower", "now", "abs",
		"concat", "substr", "replace", "btrim",
	} {
		t.Run(name, func(t *testing.T) {
			def, ok := tree.FunDefs[name]
			require.True(t, ok, "expected %q in FunDefs", name)
			require.NotEmpty(t, def.Definition, "expected overloads for %q", name)
		})
	}
}

func TestResolvedFuncDefsPopulated(t *testing.T) {
	require.NotEmpty(t, tree.ResolvedBuiltinFuncDefs)
	_, ok := tree.ResolvedBuiltinFuncDefs["pg_catalog.length"]
	require.True(t, ok, "expected pg_catalog.length in ResolvedBuiltinFuncDefs")
}

func TestAggregatesRegistered(t *testing.T) {
	def, ok := tree.FunDefs["count"]
	require.True(t, ok)
	require.NotEmpty(t, def.Definition)
	require.Equal(t, tree.AggregateClass, def.Definition[0].Class)
}
