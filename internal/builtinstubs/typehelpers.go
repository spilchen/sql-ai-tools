// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package builtinstubs

import (
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/types"
	"github.com/lib/pq/oid"
)

// oidTyp looks up a type by OID, returning types.Any for unknown OIDs.
// Shared by every generated stubs_vN_M_gen.go file. Lives here (rather
// than in the generated output) so multiple generated files can coexist
// in the same package without redeclaring the helper.
func oidTyp(o oid.Oid) *types.T {
	if t, ok := types.OidToType[o]; ok {
		return t
	}
	return types.Any
}
