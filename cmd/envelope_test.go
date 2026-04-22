// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/output"
)

func TestNewEnvelope(t *testing.T) {
	t.Run("sets tier and connection status", func(t *testing.T) {
		state := &rootState{outputFormat: output.FormatJSON}
		cmd := &cobra.Command{}
		cmd.SetOut(&bytes.Buffer{})

		r, env, err := newEnvelope(state, output.TierZeroConfig, cmd)
		require.NoError(t, err)
		require.Equal(t, output.FormatJSON, r.Format)
		require.Equal(t, output.TierZeroConfig, env.Tier)
		require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)
		require.NotEmpty(t, env.ParserVersion)
	})

	t.Run("TierUnset omits tier from JSON", func(t *testing.T) {
		state := &rootState{outputFormat: output.FormatJSON}
		cmd := &cobra.Command{}
		cmd.SetOut(&bytes.Buffer{})

		_, env, err := newEnvelope(state, output.TierUnset, cmd)
		require.NoError(t, err)
		require.Equal(t, output.TierUnset, env.Tier)

		data, err := json.Marshal(env)
		require.NoError(t, err)
		require.NotContains(t, string(data), `"tier"`)
	})
}
