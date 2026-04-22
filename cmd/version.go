// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime/debug"

	"github.com/spf13/cobra"

	// Blank import: keep cockroachdb-parser in this module's import
	// graph so `go mod tidy` does not drop it before a real subcommand
	// (validate/format/parse) imports a parser package directly. It
	// also makes the parser appear in `debug.ReadBuildInfo().Deps` for
	// `go build` artifacts. Note: `go test` binaries do NOT populate
	// Deps, so under `go test` parserVersionFrom falls back to
	// "unknown" — see TestParserVersionFrom for the synthetic-BuildInfo
	// coverage that exercises the real resolution branches.
	_ "github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"

	"github.com/spilchen/sql-ai-tools/internal/output"
)

// parserModulePath is the import path we look up in build info to
// report the cockroachdb-parser version. The `replace` directive does
// not change the recorded module path, so this constant must match the
// top-level require in go.mod.
const parserModulePath = "github.com/cockroachdb/cockroachdb-parser"

// devVersion is the sentinel Version uses for unstamped local builds.
// parserVersionFrom uses it to decide whether a missing parser dep is a
// soft "unknown" (acceptable under `go test` / `go run`) or a hard
// error (a stamped release binary that lost the blank import).
const devVersion = "dev"

// Version is the binary version string. It defaults to devVersion for
// local builds and is intended to be stamped at link time via:
//
//	go build -ldflags "-X github.com/spilchen/sql-ai-tools/cmd.Version=<version>"
//
// Release tooling owns this value; do not mutate it from runtime code.
var Version = devVersion

func newVersionCmd(state *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print binary and parser versions",
		Long: `Print the crdb-sql binary version and the resolved
cockroachdb-parser module version. The parser version is read from the
embedded Go build info, so it reflects whatever go.mod (including any
replace directive) selected at build time.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, env, err := newEnvelope(state, output.TierUnset, cmd)
			if err != nil {
				return r.RenderError(env, err)
			}
			data, err := json.Marshal(struct {
				BinaryVersion string `json:"binary_version"`
			}{BinaryVersion: Version})
			if err != nil {
				return r.RenderError(env, err)
			}
			env.Data = data
			return r.Render(env, func(w io.Writer) error {
				_, werr := fmt.Fprintf(w, "crdb-sql: %s\ncockroachdb-parser: %s\n", Version, env.ParserVersion)
				return werr
			})
		},
	}
}

// parserVersion resolves the cockroachdb-parser module version for the
// running binary. binVersion is the binary's own Version: dev builds
// tolerate a missing dep (returns "unknown") because `go test` and
// `go run` do not populate build-info Deps; a stamped release build
// instead returns an error so a regression that drops the blank import
// surfaces as a non-zero exit rather than silent "unknown" output.
func parserVersion(binVersion string) (string, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		if binVersion == devVersion {
			return "unknown", nil
		}
		return "", errors.New("build info unavailable")
	}
	return parserVersionFrom(info, binVersion)
}

// parserVersionFrom is the testable core of parserVersion. It consumes
// a *debug.BuildInfo so each resolution branch (replace-version wins,
// dep-version fallback, dep absent, dep present but no usable version)
// can be exercised in unit tests without depending on `go build`
// populating Deps.
//
// When go.mod uses a `replace` directive, the matching dep's own
// Version field is the placeholder
// "v0.0.0-00010101000000-000000000000"; the real version lives on
// dep.Replace. The replacement wins when present.
func parserVersionFrom(info *debug.BuildInfo, binVersion string) (string, error) {
	for _, dep := range info.Deps {
		if dep.Path != parserModulePath {
			continue
		}
		if dep.Replace != nil && dep.Replace.Version != "" {
			return dep.Replace.Version, nil
		}
		if dep.Version != "" {
			return dep.Version, nil
		}
		// Dep recorded but neither slot has a usable version. Treat
		// as missing rather than reporting a confusing empty string.
		break
	}
	if binVersion == devVersion {
		return "unknown", nil
	}
	return "", fmt.Errorf("parser module %q missing from build info; blank import may have been dropped", parserModulePath)
}
