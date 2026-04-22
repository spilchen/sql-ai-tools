// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// extract-builtins iterates all registered CockroachDB builtin functions
// and emits a JSON catalog of function metadata (name, param types, return
// type, volatility, class). The JSON is consumed by cmd/gen-builtins to
// produce Go registration stubs for the sql-ai-tools parser.
//
// This program must be compiled within the cockroach repo's module context
// because the builtins package transitively imports most of cockroach. To
// run it, copy this file into a package directory inside the cockroach tree,
// add a BUILD.bazel file, and build with Bazel:
//
//	cp main.go $COCKROACH_SRC/pkg/cmd/extract-builtins/
//	cd $COCKROACH_SRC && bazel run //pkg/cmd/extract-builtins \
//	  > /path/to/sql-ai-tools/internal/builtinstubs/testdata/crdb_builtins_v26.2.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/sql/sem/builtins"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/builtins/builtinsregistry"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
)

type catalog struct {
	Version     string     `json:"version"`
	GeneratedBy string     `json:"generated_by"`
	Functions   []function `json:"functions"`
}

type function struct {
	Name       string     `json:"name"`
	Properties properties `json:"properties"`
	Overloads  []overload `json:"overloads"`
}

type properties struct {
	Category                string `json:"category,omitempty"`
	AvailableOnPublicSchema bool   `json:"available_on_public_schema,omitempty"`
	Undocumented            bool   `json:"undocumented,omitempty"`
	Private                 bool   `json:"private,omitempty"`
}

type overload struct {
	Params         []param `json:"params,omitempty"`
	ParamKind      string  `json:"param_kind"`
	VariadicType   *typ    `json:"variadic_type,omitempty"`
	ReturnType     *typ    `json:"return_type,omitempty"`
	ReturnTypeKind string  `json:"return_type_kind"`
	Volatility     string  `json:"volatility"`
	Class          string  `json:"class"`
	Info           string  `json:"info,omitempty"`
	CalledOnNull   bool    `json:"called_on_null_input,omitempty"`
	Preference     int     `json:"preference,omitempty"`
}

type param struct {
	Name string `json:"name,omitempty"`
	Type typ    `json:"type"`
}

type typ struct {
	OID    uint32 `json:"oid"`
	Name   string `json:"name"`
	Family string `json:"family,omitempty"`
}

func serializeType(t *types.T) typ {
	if t == nil {
		return typ{Name: "any"}
	}
	name := t.String()
	func() {
		defer func() { _ = recover() }()
		name = t.SQLStandardName()
	}()
	return typ{
		OID:    uint32(t.Oid()),
		Name:   name,
		Family: string(t.Family().Name()),
	}
}

func volatilityString(o tree.Overload) string {
	return strings.ToLower(o.Volatility.String())
}

func classString(c tree.FunctionClass) string {
	switch c {
	case tree.NormalClass:
		return "normal"
	case tree.AggregateClass:
		return "aggregate"
	case tree.WindowClass:
		return "window"
	case tree.GeneratorClass:
		return "generator"
	default:
		return "normal"
	}
}

func serializeOverload(o *tree.Overload) overload {
	ov := overload{
		Volatility:   volatilityString(*o),
		Class:        classString(o.Class),
		Info:         o.Info,
		CalledOnNull: o.CalledOnNullInput,
		Preference:   int(o.OverloadPreference),
	}

	switch t := o.Types.(type) {
	case tree.ParamTypes:
		ov.ParamKind = "fixed"
		ov.Params = make([]param, len(t))
		for i, p := range t {
			ov.Params[i] = param{
				Name: p.Name,
				Type: serializeType(p.Typ),
			}
		}
	case tree.HomogeneousType:
		ov.ParamKind = "homogeneous"
	case tree.VariadicType:
		ov.ParamKind = "variadic"
		ov.Params = make([]param, len(t.FixedTypes))
		for i, ft := range t.FixedTypes {
			ov.Params[i] = param{Type: serializeType(ft)}
		}
		vt := serializeType(t.VarType)
		ov.VariadicType = &vt
	default:
		ov.ParamKind = "unknown"
	}

	var retTyp *types.T
	func() {
		defer func() { _ = recover() }()
		retTyp = o.FixedReturnType()
	}()
	if retTyp != nil && !retTyp.IsAmbiguous() {
		ov.ReturnTypeKind = "fixed"
		rt := serializeType(retTyp)
		ov.ReturnType = &rt
	} else {
		ov.ReturnTypeKind = "inferred"
		fallback := serializeType(types.Any)
		ov.ReturnType = &fallback
	}

	return ov
}

func main() {
	version := flag.String("version", "v26.2", "CRDB version label for the catalog")
	flag.Parse()

	names := builtins.AllBuiltinNames()
	sort.Strings(names)

	var funcs []function
	for _, name := range names {
		props, overloads := builtinsregistry.GetBuiltinProperties(name)
		if props == nil {
			continue
		}
		if props.Private {
			continue
		}

		f := function{
			Name: name,
			Properties: properties{
				Category:                props.Category,
				AvailableOnPublicSchema: props.AvailableOnPublicSchema,
				Undocumented:            props.Undocumented,
				Private:                 props.Private,
			},
		}

		for i := range overloads {
			f.Overloads = append(f.Overloads, serializeOverload(&overloads[i]))
		}

		funcs = append(funcs, f)
	}

	cat := catalog{
		Version:     *version,
		GeneratedBy: "extract-builtins",
		Functions:   funcs,
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cat); err != nil {
		fmt.Fprintf(os.Stderr, "error encoding JSON: %v\n", err)
		os.Exit(1)
	}
}
