// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/versionroute"
)

// newVersionsCmd implements the `versions` subcommand: a discovery
// view over every crdb-sql backend reachable from this process. It is
// the user-facing counterpart to the routing logic in MaybeReexec —
// when a missing-backend error suggests installing a sibling, this
// command is how users confirm what they already have.
//
// Distinct from `version` (singular), which reports just the running
// binary's parser/builtins versions. The two coexist; users will
// reach for whichever makes sense.
func newVersionsCmd(state *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "versions",
		Short: "List all installed crdb-sql backends and their CRDB quarters",
		Long: `List every crdb-sql-vXXX backend reachable from this process —
the running binary itself plus any siblings found alongside it or in
$PATH. Use this to confirm which CRDB quarters are installed before
running with --target-version, or to debug a "backend not found" error
from the router.

Output is sorted newest-quarter first. The "this binary" entry marks
which backend is currently executing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, env, err := newEnvelope(state, output.TierUnset, cmd)
			if err != nil {
				return r.RenderError(env, err)
			}
			backends := versionroute.Discover()

			type entry struct {
				Quarter string `json:"quarter,omitempty"`
				Backend string `json:"backend"`
				Path    string `json:"path"`
				IsSelf  bool   `json:"is_self"`
			}
			out := make([]entry, 0, len(backends))
			for _, b := range backends {
				out = append(out, entry{
					Quarter: b.Quarter.String(), // "unknown" for the zero value
					Backend: b.Quarter.BackendName(),
					Path:    b.Path,
					IsSelf:  b.IsSelf,
				})
			}
			data, err := json.Marshal(struct {
				Backends []entry `json:"backends"`
			}{Backends: out})
			if err != nil {
				return r.RenderError(env, err)
			}
			env.Data = data
			return r.Render(env, func(w io.Writer) error {
				if len(out) == 0 {
					_, werr := fmt.Fprintln(w, "no backends discovered")
					return werr
				}
				for _, e := range out {
					marker := "sibling    "
					if e.IsSelf {
						marker = "this binary"
					}
					if _, werr := fmt.Fprintf(w, "%s  %-15s  %-10s  %s\n",
						marker, e.Backend, e.Quarter, e.Path); werr != nil {
						return werr
					}
				}
				return nil
			})
		},
	}
}
