// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package sqlinput resolves SQL text from the three input sources every
// SQL-consuming subcommand supports: an inline -e expression, a
// positional file argument, or standard input. The resolution logic
// lives here so validate, format, parse, and future subcommands share
// one implementation and one set of error messages.
package sqlinput

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// ReadSQL resolves SQL input from three sources in priority order:
//
//  1. expr — the -e / --expression flag value (inline SQL).
//  2. args — a single positional argument treated as a file path.
//  3. stdin — standard input, read until EOF.
//
// If expr is non-empty, args must be empty (returning an error
// otherwise). If both expr and args are empty, stdin is consumed. An
// error is returned when the resolved input is empty after trimming
// whitespace.
//
// stdin is an io.Reader rather than *os.File so callers can inject a
// bytes.Buffer or strings.Reader in tests. The cobra command passes
// cmd.InOrStdin().
func ReadSQL(expr string, args []string, stdin io.Reader) (string, error) {
	if expr != "" {
		if len(args) > 0 {
			return "", fmt.Errorf("cannot use -e flag and file argument together")
		}
		return expr, nil
	}

	if len(args) > 0 {
		data, err := os.ReadFile(args[0])
		if err != nil {
			return "", fmt.Errorf("read SQL file: %w", err)
		}
		sql := string(data)
		if strings.TrimSpace(sql) == "" {
			return "", fmt.Errorf("no SQL input provided; file %q is empty", args[0])
		}
		return sql, nil
	}

	if stdin == nil {
		return "", fmt.Errorf("no SQL input provided; use -e, pass a file, or pipe to stdin")
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	sql := string(data)
	if strings.TrimSpace(sql) == "" {
		return "", fmt.Errorf("no SQL input provided; use -e, pass a file, or pipe to stdin")
	}
	return sql, nil
}
