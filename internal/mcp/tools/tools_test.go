// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package tools

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/risk"
	"github.com/spilchen/sql-ai-tools/internal/sqlparse"
)

const testParserVersion = "v0.26.2"

// requireEnvelope asserts that res is a successful tool result (not a
// tool-level error), extracts the TextContent, and unmarshals it into
// an output.Envelope.
func requireEnvelope(t *testing.T, res *mcpgo.CallToolResult) output.Envelope {
	t.Helper()
	require.False(t, res.IsError, "expected successful tool result, got tool-level error")
	require.Len(t, res.Content, 1, "expected exactly one content block")
	text, ok := res.Content[0].(mcpgo.TextContent)
	require.True(t, ok, "expected TextContent, got %T", res.Content[0])
	var env output.Envelope
	require.NoError(t, json.Unmarshal([]byte(text.Text), &env))
	return env
}

// TestExplainSQLHandlerParameterValidation covers the tool-level error
// path for explain_sql. Cluster round-trips are not exercised here
// because the handler does not gain new logic beyond parameter
// validation and conn.Manager wiring; an end-to-end smoke is documented
// in the issue verification plan.
func TestExplainSQLHandlerParameterValidation(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
	}{
		{name: "missing both params", args: map[string]any{}},
		{name: "missing dsn", args: map[string]any{"sql": "SELECT 1"}},
		{name: "missing sql", args: map[string]any{"dsn": "postgres://h:26257/db"}},
		{name: "empty sql", args: map[string]any{"sql": "", "dsn": "postgres://h:26257/db"}},
		{name: "empty dsn", args: map[string]any{"sql": "SELECT 1", "dsn": ""}},
		{name: "wrong type sql", args: map[string]any{"sql": 1, "dsn": "postgres://h:26257/db"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := ExplainSQLHandler(testParserVersion, "" /* defaultTargetVersion */)
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args

			res, err := handler(context.Background(), req)
			require.NoError(t, err)
			require.NotNil(t, res)
			require.True(t, res.IsError, "expected tool-level error")
		})
	}
}

// TestExplainSQLHandlerRejectsMalformedTargetVersion confirms a
// per-call target_version that fails validation is rejected as a
// tool error before any cluster dial is attempted. The Tier 3 path
// shares resolveTargetVersion with the Tier 1 handlers, but explain
// has the additional cost of a network attempt — so this case
// specifically pins that validation runs first.
func TestExplainSQLHandlerRejectsMalformedTargetVersion(t *testing.T) {
	handler := ExplainSQLHandler(testParserVersion, "" /* defaultTargetVersion */)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"sql":            "SELECT 1",
		"dsn":            "postgres://nope:1/db?connect_timeout=1",
		"target_version": "garbage",
	}

	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.IsError, "malformed target_version must surface as a tool error")
}

// TestExplainSQLToolAdvertisesTargetVersionParam pins that the MCP
// schema for explain_sql lists target_version among its parameters,
// so clients can discover the override surface from tool metadata
// without reading the source.
func TestExplainSQLToolAdvertisesTargetVersionParam(t *testing.T) {
	tool := ExplainSQLTool()
	props := tool.InputSchema.Properties
	require.Contains(t, props, TargetVersionParamName,
		"explain_sql schema must advertise the target_version property")
}

// TestExplainSQLHandlerConnectionFailureSurfacesEnvelopeError verifies
// that when the cluster is unreachable, the handler returns a successful
// MCP tool result whose envelope carries the error — not a tool-level
// error. This is the discipline documented in tools.go: SQL/cluster
// problems live in the envelope so agents can read them uniformly.
func TestExplainSQLHandlerConnectionFailureSurfacesEnvelopeError(t *testing.T) {
	handler := ExplainSQLHandler(testParserVersion, "" /* defaultTargetVersion */)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"sql": "SELECT 1",
		// Unreachable host — connection will fail before any query.
		"dsn": "postgres://nope:1/db?connect_timeout=1",
	}

	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	env := requireEnvelope(t, res)
	require.Equal(t, output.TierConnected, env.Tier)
	require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus,
		"failed connect must leave status disconnected")
	require.Len(t, env.Errors, 1)
	require.Contains(t, env.Errors[0].Message, "connect to CockroachDB")
}

func TestExtractSQL(t *testing.T) {
	tests := []struct {
		name        string
		args        map[string]any
		expectedSQL string
		expectedErr bool
	}{
		{name: "valid string", args: map[string]any{"sql": "SELECT 1"}, expectedSQL: "SELECT 1"},
		{name: "missing param", args: map[string]any{}, expectedErr: true},
		{name: "wrong type", args: map[string]any{"sql": 42}, expectedErr: true},
		{name: "empty string", args: map[string]any{"sql": ""}, expectedErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args

			sql, toolErr := extractSQL(req)
			if tc.expectedErr {
				require.NotNil(t, toolErr, "expected tool error")
				require.Empty(t, sql)
			} else {
				require.Nil(t, toolErr, "expected no tool error")
				require.Equal(t, tc.expectedSQL, sql)
			}
		})
	}
}

func TestParseSQLHandler(t *testing.T) {
	tests := []struct {
		name              string
		args              map[string]any
		expectedToolErr   bool
		expectedEnvErrs   bool
		expectedCode      string
		expectedStmtCount int
		expectedType      string
		expectedTag       string
	}{
		{
			name:              "single DML",
			args:              map[string]any{"sql": "SELECT 1"},
			expectedStmtCount: 1,
			expectedType:      "DML",
			expectedTag:       "SELECT",
		},
		{
			name:              "multi-statement",
			args:              map[string]any{"sql": "SELECT 1; BEGIN"},
			expectedStmtCount: 2,
		},
		{
			name:            "parse error returns structured SQLSTATE error",
			args:            map[string]any{"sql": "SELECTT 1"},
			expectedEnvErrs: true,
			expectedCode:    "42601",
		},
		{
			name:            "missing sql param",
			args:            map[string]any{},
			expectedToolErr: true,
		},
		{
			name:            "empty sql",
			args:            map[string]any{"sql": ""},
			expectedToolErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := ParseSQLHandler(testParserVersion, "" /* defaultTargetVersion */)
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args

			res, err := handler(context.Background(), req)
			require.NoError(t, err)
			require.NotNil(t, res)

			if tc.expectedToolErr {
				require.True(t, res.IsError, "expected tool-level error")
				return
			}

			env := requireEnvelope(t, res)
			require.Equal(t, testParserVersion, env.ParserVersion)
			require.Equal(t, output.TierZeroConfig, env.Tier)
			require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)

			if tc.expectedEnvErrs {
				require.NotEmpty(t, env.Errors)
				require.Nil(t, env.Data)
				if tc.expectedCode != "" {
					require.Equal(t, tc.expectedCode, env.Errors[0].Code)
				}
				return
			}

			require.Empty(t, env.Errors)
			var stmts []sqlparse.ClassifiedStatement
			require.NoError(t, json.Unmarshal(env.Data, &stmts))
			require.Len(t, stmts, tc.expectedStmtCount)

			if tc.expectedType != "" {
				require.Equal(t, tc.expectedType, string(stmts[0].StatementType))
				require.Equal(t, tc.expectedTag, stmts[0].Tag)
			}
		})
	}
}

func TestValidateSQLHandler(t *testing.T) {
	tests := []struct {
		name            string
		args            map[string]any
		expectedToolErr bool
		expectedValid   bool
		expectedEnvErrs bool
		expectedCode    string
	}{
		{
			name:          "valid SQL",
			args:          map[string]any{"sql": "SELECT 1"},
			expectedValid: true,
		},
		{
			name:            "syntax error",
			args:            map[string]any{"sql": "SELECT FROM"},
			expectedEnvErrs: true,
			expectedCode:    "42601",
		},
		{
			name:            "type mismatch",
			args:            map[string]any{"sql": "SELECT 1 + 'hello'"},
			expectedEnvErrs: true,
		},
		{
			name:          "column ref does not false-positive",
			args:          map[string]any{"sql": "SELECT a + 1 FROM t"},
			expectedValid: true,
		},
		{
			name:          "whitespace trimmed",
			args:          map[string]any{"sql": "  SELECT 1  \n"},
			expectedValid: true,
		},
		{
			name:            "missing sql param",
			args:            map[string]any{},
			expectedToolErr: true,
		},
		{
			name:            "empty sql",
			args:            map[string]any{"sql": ""},
			expectedToolErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := ValidateSQLHandler(testParserVersion, "" /* defaultTargetVersion */)
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args

			res, err := handler(context.Background(), req)
			require.NoError(t, err)
			require.NotNil(t, res)

			if tc.expectedToolErr {
				require.True(t, res.IsError, "expected tool-level error")
				return
			}

			env := requireEnvelope(t, res)
			require.Equal(t, testParserVersion, env.ParserVersion)
			require.Equal(t, output.TierZeroConfig, env.Tier)
			require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)

			if tc.expectedEnvErrs {
				require.NotEmpty(t, env.Errors)
				require.Nil(t, env.Data)
				if tc.expectedCode != "" {
					require.Equal(t, tc.expectedCode, env.Errors[0].Code)
				}
				return
			}

			require.Empty(t, env.Errors)
			require.NotNil(t, env.Data)

			if tc.expectedValid {
				var data struct {
					Valid bool `json:"valid"`
				}
				require.NoError(t, json.Unmarshal(env.Data, &data))
				require.True(t, data.Valid)
			}
		})
	}
}

func TestFormatSQLHandler(t *testing.T) {
	tests := []struct {
		name              string
		args              map[string]any
		expectedToolErr   bool
		expectedEnvErrs   bool
		expectedCode      string
		expectedFormatted string
	}{
		{
			name:              "basic formatting",
			args:              map[string]any{"sql": "select  1"},
			expectedFormatted: "SELECT 1",
		},
		{
			name:              "multi-statement",
			args:              map[string]any{"sql": "select 1; select 2"},
			expectedFormatted: "SELECT 1;\nSELECT 2",
		},
		{
			name:            "parse error returns structured SQLSTATE error",
			args:            map[string]any{"sql": "SELECTT 1"},
			expectedEnvErrs: true,
			expectedCode:    "42601",
		},
		{
			name:            "missing sql param",
			args:            map[string]any{},
			expectedToolErr: true,
		},
		{
			name:            "empty sql",
			args:            map[string]any{"sql": ""},
			expectedToolErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := FormatSQLHandler(testParserVersion, "" /* defaultTargetVersion */)
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args

			res, err := handler(context.Background(), req)
			require.NoError(t, err)
			require.NotNil(t, res)

			if tc.expectedToolErr {
				require.True(t, res.IsError, "expected tool-level error")
				return
			}

			env := requireEnvelope(t, res)
			require.Equal(t, testParserVersion, env.ParserVersion)
			require.Equal(t, output.TierZeroConfig, env.Tier)
			require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)

			if tc.expectedEnvErrs {
				require.NotEmpty(t, env.Errors)
				require.Nil(t, env.Data)
				if tc.expectedCode != "" {
					require.Equal(t, tc.expectedCode, env.Errors[0].Code)
				}
				return
			}

			require.Empty(t, env.Errors)
			require.NotNil(t, env.Data)

			var data struct {
				FormattedSQL string `json:"formatted_sql"`
			}
			require.NoError(t, json.Unmarshal(env.Data, &data))
			require.Equal(t, tc.expectedFormatted, data.FormattedSQL)
		})
	}
}

func TestDetectRiskyQueryHandler(t *testing.T) {
	tests := []struct {
		name                 string
		args                 map[string]any
		expectedToolErr      bool
		expectedEnvErrs      bool
		expectedCode         string
		expectedFindingCount int
		expectedReasonCode   string
	}{
		{
			name:                 "risky DELETE",
			args:                 map[string]any{"sql": "DELETE FROM users"},
			expectedFindingCount: 1,
			expectedReasonCode:   "DELETE_NO_WHERE",
		},
		{
			name:                 "safe SELECT",
			args:                 map[string]any{"sql": "SELECT id FROM t WHERE id = 1"},
			expectedFindingCount: 0,
		},
		{
			name:                 "multiple findings",
			args:                 map[string]any{"sql": "DELETE FROM t; SELECT * FROM t"},
			expectedFindingCount: 2,
		},
		{
			name:            "parse error returns structured SQLSTATE error",
			args:            map[string]any{"sql": "SELECTT 1"},
			expectedEnvErrs: true,
			expectedCode:    "42601",
		},
		{
			name:            "empty sql",
			args:            map[string]any{"sql": ""},
			expectedToolErr: true,
		},
		{
			name:            "missing sql param",
			args:            map[string]any{},
			expectedToolErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := DetectRiskyQueryHandler(testParserVersion, "" /* defaultTargetVersion */)
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args

			res, err := handler(context.Background(), req)
			require.NoError(t, err)
			require.NotNil(t, res)

			if tc.expectedToolErr {
				require.True(t, res.IsError, "expected tool-level error")
				return
			}

			env := requireEnvelope(t, res)
			require.Equal(t, testParserVersion, env.ParserVersion)
			require.Equal(t, output.TierZeroConfig, env.Tier)
			require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)

			if tc.expectedEnvErrs {
				require.NotEmpty(t, env.Errors)
				require.Nil(t, env.Data)
				if tc.expectedCode != "" {
					require.Equal(t, tc.expectedCode, env.Errors[0].Code)
				}
				return
			}

			require.Empty(t, env.Errors)
			require.NotNil(t, env.Data)

			var findings []risk.Finding
			require.NoError(t, json.Unmarshal(env.Data, &findings))
			require.Len(t, findings, tc.expectedFindingCount)

			if tc.expectedReasonCode != "" {
				require.Equal(t, tc.expectedReasonCode, findings[0].ReasonCode)
				require.Equal(t, risk.SeverityCritical, findings[0].Severity)
				require.NotEmpty(t, findings[0].FixHint)
			}
		})
	}
}

// TestResolveTargetVersion locks the precedence rule (per-call argument
// beats server default), the empty-string-means-default convention, and
// the input-validation contract. The wire-level effect of the chosen
// value (envelope stamping + warning) is covered separately in
// TestBaseEnvelope and the per-tool integration cases below.
func TestResolveTargetVersion(t *testing.T) {
	tests := []struct {
		name                  string
		args                  map[string]any
		defaultTargetVersion  string
		expectedTargetVersion string
		expectedToolErr       bool
	}{
		{
			name:                  "no arg uses default",
			args:                  map[string]any{},
			defaultTargetVersion:  "25.4.0",
			expectedTargetVersion: "25.4.0",
		},
		{
			name:                  "no arg and no default yields empty",
			args:                  map[string]any{},
			defaultTargetVersion:  "",
			expectedTargetVersion: "",
		},
		{
			name:                  "per-call arg overrides default",
			args:                  map[string]any{"target_version": "26.1.0"},
			defaultTargetVersion:  "25.4.0",
			expectedTargetVersion: "26.1.0",
		},
		{
			name:                  "per-call arg canonicalizes leading v",
			args:                  map[string]any{"target_version": "v26.1.0"},
			defaultTargetVersion:  "",
			expectedTargetVersion: "26.1.0",
		},
		{
			// Pins that override and canonicalization compose; a
			// future short-circuit that returns the per-call arg
			// raw when a default is also set would slip past
			// either case in isolation.
			name:                  "per-call arg with leading v beats default and canonicalizes",
			args:                  map[string]any{"target_version": "v26.1.0"},
			defaultTargetVersion:  "25.4.0",
			expectedTargetVersion: "26.1.0",
		},
		{
			name:                  "empty per-call arg falls through to default",
			args:                  map[string]any{"target_version": ""},
			defaultTargetVersion:  "25.4.0",
			expectedTargetVersion: "25.4.0",
		},
		{
			name:                 "non-string per-call arg returns tool error",
			args:                 map[string]any{"target_version": 25},
			defaultTargetVersion: "",
			expectedToolErr:      true,
		},
		{
			name:                 "malformed per-call arg returns tool error",
			args:                 map[string]any{"target_version": "garbage"},
			defaultTargetVersion: "",
			expectedToolErr:      true,
		},
		{
			name:                 "leading whitespace on per-call arg is tolerated",
			args:                 map[string]any{"target_version": "  25.4.0  "},
			defaultTargetVersion: "",
			// resolveTargetVersion both trims itself and forwards
			// to ValidateTargetVersion (which also trims), so this
			// pins that whitespace handling matches the CLI.
			expectedTargetVersion: "25.4.0",
		},
		{
			// Pins that "v" alone is rejected as malformed rather
			// than canonicalized to "" and silently falling
			// through to the default.
			name:                 "v-only per-call arg returns tool error",
			args:                 map[string]any{"target_version": "v"},
			defaultTargetVersion: "25.4.0",
			expectedToolErr:      true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args

			got, toolErr := resolveTargetVersion(req, tc.defaultTargetVersion)
			if tc.expectedToolErr {
				require.NotNil(t, toolErr)
				return
			}
			require.Nil(t, toolErr)
			require.Equal(t, tc.expectedTargetVersion, got)
		})
	}
}

// TestResolveTargetVersionErrorMentionsParam pins that tool errors
// returned for a malformed target_version argument include the
// parameter name. Clients invoking a tool with several validated
// arguments need to know *which* one was rejected.
func TestResolveTargetVersionErrorMentionsParam(t *testing.T) {
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"target_version": "garbage"}

	_, toolErr := resolveTargetVersion(req, "")
	require.NotNil(t, toolErr)
	require.True(t, toolErr.IsError)
	require.Len(t, toolErr.Content, 1)
	text, ok := toolErr.Content[0].(mcpgo.TextContent)
	require.True(t, ok)
	require.Contains(t, text.Text, TargetVersionParamName)
}

// TestBaseEnvelope pins the wire-level effect of targetVersion: empty
// → no field, no warning; non-empty matching parser → field stamped,
// no warning; non-empty mismatching parser → field stamped + warning
// appended. Each tool handler delegates to this helper, so covering it
// once spares per-tool duplication.
func TestBaseEnvelope(t *testing.T) {
	t.Run("empty target version omits field and emits no warning", func(t *testing.T) {
		env := baseEnvelope("v0.26.2", "")
		require.Equal(t, output.TierZeroConfig, env.Tier)
		require.Equal(t, "v0.26.2", env.ParserVersion)
		require.Empty(t, env.TargetVersion)
		require.Empty(t, env.Errors)
	})

	t.Run("matching target version stamps field with no warning", func(t *testing.T) {
		env := baseEnvelope("v0.26.2", "0.26.5")
		require.Equal(t, "0.26.5", env.TargetVersion)
		require.Empty(t, env.Errors,
			"matching MAJOR.MINOR must not produce a warning")
	})

	t.Run("mismatched target version stamps field and appends warning", func(t *testing.T) {
		env := baseEnvelope("v0.26.2", "1.0.0")
		require.Equal(t, "1.0.0", env.TargetVersion)
		require.Len(t, env.Errors, 1)
		require.Equal(t, "target_version_mismatch", env.Errors[0].Code)
		require.Equal(t, output.SeverityWarning, env.Errors[0].Severity)
	})
}

// TestParseSQLHandlerTargetVersion confirms that the per-call
// target_version argument lands on the envelope returned by an actual
// tool handler. The other three Tier 1 handlers go through the same
// resolve+stamp helpers, so this single end-to-end check protects the
// whole MCP wiring without quadrupling the table sizes above.
func TestParseSQLHandlerTargetVersion(t *testing.T) {
	tests := []struct {
		name                  string
		args                  map[string]any
		defaultTargetVersion  string
		expectedTargetVersion string
		expectedToolErr       bool
	}{
		{
			name:                  "default applies when no per-call arg",
			args:                  map[string]any{"sql": "SELECT 1"},
			defaultTargetVersion:  "25.4.0",
			expectedTargetVersion: "25.4.0",
		},
		{
			name: "per-call arg overrides default",
			args: map[string]any{
				"sql":            "SELECT 1",
				"target_version": "26.1.0",
			},
			defaultTargetVersion:  "25.4.0",
			expectedTargetVersion: "26.1.0",
		},
		{
			name:                  "no arg and no default omits field",
			args:                  map[string]any{"sql": "SELECT 1"},
			defaultTargetVersion:  "",
			expectedTargetVersion: "",
		},
		{
			name: "malformed per-call arg returns tool error",
			args: map[string]any{
				"sql":            "SELECT 1",
				"target_version": "garbage",
			},
			defaultTargetVersion: "",
			expectedToolErr:      true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := ParseSQLHandler(testParserVersion, tc.defaultTargetVersion)
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args

			res, err := handler(context.Background(), req)
			require.NoError(t, err)
			require.NotNil(t, res)

			if tc.expectedToolErr {
				require.True(t, res.IsError)
				return
			}

			env := requireEnvelope(t, res)
			require.Equal(t, tc.expectedTargetVersion, env.TargetVersion)
		})
	}
}
