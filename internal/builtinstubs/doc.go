// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package builtinstubs registers metadata-only stubs for CockroachDB
// builtin functions into the parser's builtins registry. The stubs
// contain function signatures (parameter types, return type, volatility,
// function class) but no execution closures, enabling function-name
// validation, overload resolution, and type-checking of function call
// arguments without a live database connection.
//
// Stubs are generated from CockroachDB source by cmd/extract-builtins
// (which dumps a JSON catalog) and cmd/gen-builtins (which emits Go
// registration code). Multiple CRDB versions can be compiled in; the
// caller selects one via Init before any SQL is parsed.
//
// Typical usage from cmd/crdb-sql/main.go:
//
//	builtinstubs.Init("")  // registers the default version (matches parser)
package builtinstubs
