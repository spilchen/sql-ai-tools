// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package builtinstubs

import (
	"fmt"
	"sort"
	"strings"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/builtins/builtinsregistry"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/catconstants"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/tree"
	"github.com/lib/pq/oid"
)

// DefaultVersion is the CRDB version whose stubs are registered when
// Init is called with an empty string. It should match the
// cockroachdb-parser module version pinned in go.mod.
const DefaultVersion = "v26.2"

// versionRegistry maps CRDB version strings to their registration
// functions. Each entry is added by a generated stubs file.
var versionRegistry = map[string]func(){
	"v26.2": registerV26_2,
	"v26.1": registerV26_1,
}

var activeVersion string

// Init registers builtin stubs for the given CRDB version. It must be
// called exactly once, before any SQL parsing occurs. Pass "" to use
// DefaultVersion. Panics if the version is not compiled in.
func Init(version string) {
	if version == "" {
		version = DefaultVersion
	}
	fn, ok := versionRegistry[version]
	if !ok {
		panic(fmt.Sprintf("builtinstubs: unknown version %q; available: %s",
			version, strings.Join(SupportedVersions(), ", ")))
	}
	fn()
	activeVersion = version
}

// ActiveVersion returns the version that was registered by Init, or
// "" if Init has not been called.
func ActiveVersion() string { return activeVersion }

// SupportedVersions returns all compiled-in stub versions, sorted.
func SupportedVersions() []string {
	vs := make([]string, 0, len(versionRegistry))
	for v := range versionRegistry {
		vs = append(vs, v)
	}
	sort.Strings(vs)
	return vs
}

func init() {
	tree.FunDefs = make(map[string]*tree.FunctionDefinition)
	tree.ResolvedBuiltinFuncDefs = make(map[string]*tree.ResolvedFunctionDefinition)
	tree.OidToQualifiedBuiltinOverload = make(map[oid.Oid]tree.QualifiedOverload)
	tree.OidToBuiltinName = make(map[oid.Oid]string)

	builtinsregistry.AddSubscription(func(name string, props *tree.FunctionProperties, overloads []tree.Overload) {
		fDef := tree.NewFunctionDefinition(name, props, overloads)
		tree.FunDefs[name] = fDef
		addResolvedFuncDef(tree.ResolvedBuiltinFuncDefs, fDef)
	})
}

// addResolvedFuncDef populates ResolvedBuiltinFuncDefs so that
// function resolution (tree.GetBuiltinFuncDefinition) can find
// builtins by name. Mirrors the logic in cockroach's
// all_builtins.go, simplified: we skip OID assignment and cast
// builtin tracking since we never execute queries.
func addResolvedFuncDef(
	resolved map[string]*tree.ResolvedFunctionDefinition,
	def *tree.FunctionDefinition,
) {
	parts := strings.Split(def.Name, ".")
	if len(parts) == 2 {
		resolved[def.Name] = tree.QualifyBuiltinFunctionDefinition(def, parts[0])
		return
	}
	pgName := catconstants.PgCatalogName + "." + def.Name
	resolved[pgName] = tree.QualifyBuiltinFunctionDefinition(def, catconstants.PgCatalogName)
	if def.AvailableOnPublicSchema {
		pubName := catconstants.PublicSchemaName + "." + def.Name
		resolved[pubName] = tree.QualifyBuiltinFunctionDefinition(def, catconstants.PublicSchemaName)
	}
}
