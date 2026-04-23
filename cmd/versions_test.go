// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestVersionsCmd exercises the `versions` subcommand and confirms it
// always reports the running test binary as a backend (with IsSelf
// true). Sibling discovery is covered in
// internal/versionroute/discover_test.go; this test only verifies the
// cobra wiring and JSON shape.
func TestVersionsCmd(t *testing.T) {
	t.Setenv("PATH", "")
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"versions", "-o", "json"})

	require.NoError(t, root.Execute())

	var env struct {
		Data struct {
			Backends []struct {
				Quarter string `json:"quarter"`
				Backend string `json:"backend"`
				Path    string `json:"path"`
				IsSelf  bool   `json:"is_self"`
			} `json:"backends"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &env))
	require.NotEmpty(t, env.Data.Backends, "at least the running binary must be listed")

	var selfCount int
	for _, b := range env.Data.Backends {
		if b.IsSelf {
			selfCount++
			require.NotEmpty(t, b.Path)
		}
	}
	require.Equal(t, 1, selfCount, "exactly one backend must be marked is_self")
}
