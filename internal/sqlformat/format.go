// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package sqlformat wraps the cockroachdb-parser module to provide
// SQL pretty-printing. The primary entry point is Format, which parses
// a SQL string and re-emits it in CockroachDB's canonical
// pretty-printed form using tree.DefaultPrettyCfg.
package sqlformat

import (
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/tree"
)

// Format parses sql using the CockroachDB parser and returns the
// pretty-printed canonical form. Multiple statements in the input
// are separated by ";\n" in the output. An empty input returns an
// empty string with no error. Parse or formatting errors are
// returned as a Go error; the caller decides how to surface them.
func Format(sql string) (string, error) {
	stmts, err := parser.Parse(sql)
	if err != nil {
		return "", err
	}
	if len(stmts) == 0 {
		return "", nil
	}

	cfg := tree.DefaultPrettyCfg()
	var buf strings.Builder
	for i, stmt := range stmts {
		if i > 0 {
			buf.WriteString(";\n")
		}
		pretty, err := cfg.Pretty(stmt.AST)
		if err != nil {
			return "", fmt.Errorf("format statement %d: %w", i+1, err)
		}
		buf.WriteString(pretty)
	}
	return buf.String(), nil
}
