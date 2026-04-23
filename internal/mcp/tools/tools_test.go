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

	"github.com/spilchen/sql-ai-tools/internal/builtinstubs"
	"github.com/spilchen/sql-ai-tools/internal/output"
	"github.com/spilchen/sql-ai-tools/internal/risk"
	"github.com/spilchen/sql-ai-tools/internal/sqlparse"
	"github.com/spilchen/sql-ai-tools/internal/summarize"
	"github.com/spilchen/sql-ai-tools/internal/validateresult"
)

// init populates the builtin function registry that
// semcheck.CheckFunctionNames consults. Production binaries (the CLI
// and the MCP server) call builtinstubs.Init from their main; tests
// in this package previously did not exercise the function-name path
// so the registry could be empty without consequence. Issue #107
// changed that — without this init, the unknown-function check would
// flag every name (including legitimate builtins like "length") as
// unknown.
func init() {
	builtinstubs.Init("")
}

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

// TestExplainSchemaChangeHandlerParameterValidation covers the
// tool-level error path for explain_schema_change. Cluster round-trips
// are not exercised here; an end-to-end smoke is documented in the
// issue verification plan.
func TestExplainSchemaChangeHandlerParameterValidation(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
	}{
		{name: "missing both params", args: map[string]any{}},
		{name: "missing dsn", args: map[string]any{"sql": "ALTER TABLE t ADD COLUMN x INT"}},
		{name: "missing sql", args: map[string]any{"dsn": "postgres://h:26257/db"}},
		{name: "empty sql", args: map[string]any{"sql": "", "dsn": "postgres://h:26257/db"}},
		{name: "empty dsn", args: map[string]any{"sql": "ALTER TABLE t ADD COLUMN x INT", "dsn": ""}},
		{name: "wrong type sql", args: map[string]any{"sql": 1, "dsn": "postgres://h:26257/db"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := ExplainSchemaChangeHandler(testParserVersion, "" /* defaultTargetVersion */)
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args

			res, err := handler(context.Background(), req)
			require.NoError(t, err)
			require.NotNil(t, res)
			require.True(t, res.IsError, "expected tool-level error")
		})
	}
}

// TestExplainSchemaChangeHandlerRejectsMalformedTargetVersion confirms
// per-call target_version validation runs before the cluster dial,
// matching the explain_sql behavior.
func TestExplainSchemaChangeHandlerRejectsMalformedTargetVersion(t *testing.T) {
	handler := ExplainSchemaChangeHandler(testParserVersion, "" /* defaultTargetVersion */)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"sql":            "ALTER TABLE t ADD COLUMN x INT",
		"dsn":            "postgres://nope:1/db?connect_timeout=1",
		"target_version": "garbage",
	}

	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.IsError, "malformed target_version must surface as a tool error")
}

// TestExplainSchemaChangeToolAdvertisesTargetVersionParam pins that
// the MCP schema for explain_schema_change lists target_version among
// its parameters so clients can discover the override surface from
// tool metadata.
func TestExplainSchemaChangeToolAdvertisesTargetVersionParam(t *testing.T) {
	tool := ExplainSchemaChangeTool()
	props := tool.InputSchema.Properties
	require.Contains(t, props, TargetVersionParamName,
		"explain_schema_change schema must advertise the target_version property")
}

// TestExplainSchemaChangeHandlerConnectionFailureSurfacesEnvelopeError
// verifies that when the cluster is unreachable, the handler returns a
// successful MCP tool result whose envelope carries the error — not a
// tool-level error. Same discipline as explain_sql.
func TestExplainSchemaChangeHandlerConnectionFailureSurfacesEnvelopeError(t *testing.T) {
	handler := ExplainSchemaChangeHandler(testParserVersion, "" /* defaultTargetVersion */)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"sql": "ALTER TABLE t ADD COLUMN x INT",
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

// TestValidateSQLHandlerStampsTargetVersionOnSchemaFilePath locks in
// the contract that when a client supplies both schemas and
// target_version, the resolved target is stamped onto the Tier 2
// envelope. The bug this guards against is a silent drop on the
// schema_file path when both args are present.
func TestValidateSQLHandlerStampsTargetVersionOnSchemaFilePath(t *testing.T) {
	const usersDDL = "CREATE TABLE users (id INT PRIMARY KEY, email TEXT)"

	handler := ValidateSQLHandler(testParserVersion, "" /* defaultTargetVersion */)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"sql":            "SELECT id FROM users",
		"schemas":        []any{map[string]any{"sql": usersDDL}},
		"target_version": "v25.4",
	}

	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	env := requireEnvelope(t, res)
	require.Equal(t, output.TierSchemaFile, env.Tier)
	// resolveTargetVersion canonicalises by stripping the leading "v",
	// matching the CLI's --target-version handling.
	require.Equal(t, "25.4", env.TargetVersion,
		"target_version must be stamped even on the schema_file path")
}

// TestValidateSQLHandlerFailurePayloadShapes pins the Result.Checks
// emitted on each failure path so MCP consumers can branch on the
// per-phase status without inspecting error codes. The CLI surface
// has equivalent coverage in TestValidateCmdJSONError /
// TestValidateCmdTypeErrorJSON; this test guarantees the MCP envelope
// does not drift from that contract.
func TestValidateSQLHandlerFailurePayloadShapes(t *testing.T) {
	const usersDDL = "CREATE TABLE users (id INT PRIMARY KEY)"

	tests := []struct {
		name                       string
		args                       map[string]any
		expectedSyntax             validateresult.CheckStatus
		expectedFunctionResolution validateresult.CheckStatus
		expectedTypeCheck          validateresult.CheckStatus
		expectedNameResolution     validateresult.CheckStatus
	}{
		{
			name:                       "parse error skips downstream phases",
			args:                       map[string]any{"sql": "SELECT FROM"},
			expectedSyntax:             validateresult.CheckFailed,
			expectedFunctionResolution: validateresult.CheckSkipped,
			expectedTypeCheck:          validateresult.CheckSkipped,
			expectedNameResolution:     validateresult.CheckSkipped,
		},
		{
			name:                       "type error without schemas leaves name resolution skipped",
			args:                       map[string]any{"sql": "SELECT 1 + 'x'"},
			expectedSyntax:             validateresult.CheckOK,
			expectedFunctionResolution: validateresult.CheckOK,
			expectedTypeCheck:          validateresult.CheckFailed,
			expectedNameResolution:     validateresult.CheckSkipped,
		},
		{
			name: "unknown table marks name resolution failed",
			args: map[string]any{
				"sql":     "SELECT * FROM nope",
				"schemas": []any{map[string]any{"sql": usersDDL}},
			},
			expectedSyntax:             validateresult.CheckOK,
			expectedFunctionResolution: validateresult.CheckOK,
			expectedTypeCheck:          validateresult.CheckOK,
			expectedNameResolution:     validateresult.CheckFailed,
		},
		{
			name:                       "unknown function flips function_resolution to failed",
			args:                       map[string]any{"sql": "SELECT now_()"},
			expectedSyntax:             validateresult.CheckOK,
			expectedFunctionResolution: validateresult.CheckFailed,
			expectedTypeCheck:          validateresult.CheckOK,
			expectedNameResolution:     validateresult.CheckSkipped,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := ValidateSQLHandler(testParserVersion, "" /* defaultTargetVersion */)
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = tc.args

			res, err := handler(context.Background(), req)
			require.NoError(t, err)
			env := requireEnvelope(t, res)
			require.NotNil(t, env.Data, "failure path must emit a Result payload")

			var result validateresult.Result
			require.NoError(t, json.Unmarshal(env.Data, &result))
			require.False(t, result.Valid)
			require.Equal(t, tc.expectedSyntax, result.Checks.Syntax)
			require.Equal(t, tc.expectedFunctionResolution, result.Checks.FunctionResolution)
			require.Equal(t, tc.expectedTypeCheck, result.Checks.TypeCheck)
			require.Equal(t, tc.expectedNameResolution, result.Checks.NameResolution)
		})
	}
}

// TestValidateSQLHandlerUnknownFunctionShape pins the end-to-end MCP
// envelope shape for issue #107: an unknown bare function name produces
// a 42883 error with did-you-mean suggestions and an
// available_functions sample, in addition to the failed
// function_resolution check status.
func TestValidateSQLHandlerUnknownFunctionShape(t *testing.T) {
	handler := ValidateSQLHandler(testParserVersion, "" /* defaultTargetVersion */)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"sql": "SELECT now_()"}

	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	env := requireEnvelope(t, res)

	var funcErr *output.Error
	for i := range env.Errors {
		if env.Errors[i].Code == "42883" {
			funcErr = &env.Errors[i]
			break
		}
	}
	require.NotNil(t, funcErr, "expected a 42883 diagnostic in MCP envelope")
	require.Equal(t, "unknown_function", funcErr.Category)
	require.Contains(t, funcErr.Message, "now_")
	require.NotNil(t, funcErr.Position)
	avail, ok := funcErr.Context["available_functions"].([]any)
	require.True(t, ok, "available_functions must be a JSON array")
	require.NotEmpty(t, avail)
	require.NotEmpty(t, funcErr.Suggestions)
	require.Equal(t, "now", funcErr.Suggestions[0].Replacement)

	var result validateresult.Result
	require.NoError(t, json.Unmarshal(env.Data, &result))
	require.False(t, result.Valid)
	require.Equal(t, validateresult.CheckFailed, result.Checks.FunctionResolution)
}

func TestValidateSQLHandler(t *testing.T) {
	const usersDDL = "CREATE TABLE users (id INT PRIMARY KEY, email TEXT)"

	tests := []struct {
		name                   string
		args                   map[string]any
		expectedToolErr        bool
		expectedValid          bool
		expectedEnvErrs        bool
		expectedCode           string
		expectedTier           output.Tier
		expectedNameResolution validateresult.CheckStatus
		expectCapabilityWarn   bool
		expectSchemaWarning    bool
		// expectNoFailureData is true for failure paths that pre-empt
		// validation (e.g. a malformed --schema input): the SQL was
		// never checked, so no Result payload is produced. Other
		// failure paths (parse, type, name) emit
		// {valid:false, checks:{...}} so consumers can branch on
		// Result.Checks without inspecting error codes.
		expectNoFailureData bool
	}{
		{
			name:                   "valid SQL without schema reports name_resolution skipped",
			args:                   map[string]any{"sql": "SELECT 1"},
			expectedValid:          true,
			expectedTier:           output.TierZeroConfig,
			expectedNameResolution: validateresult.CheckSkipped,
			expectCapabilityWarn:   true,
		},
		{
			name:            "syntax error",
			args:            map[string]any{"sql": "SELECT FROM"},
			expectedEnvErrs: true,
			expectedCode:    "42601",
			expectedTier:    output.TierZeroConfig,
		},
		{
			name:            "type mismatch",
			args:            map[string]any{"sql": "SELECT 1 + 'hello'"},
			expectedEnvErrs: true,
			expectedTier:    output.TierZeroConfig,
		},
		{
			name:                   "column ref does not false-positive",
			args:                   map[string]any{"sql": "SELECT a + 1 FROM t"},
			expectedValid:          true,
			expectedTier:           output.TierZeroConfig,
			expectedNameResolution: validateresult.CheckSkipped,
			expectCapabilityWarn:   true,
		},
		{
			name:                   "whitespace trimmed",
			args:                   map[string]any{"sql": "  SELECT 1  \n"},
			expectedValid:          true,
			expectedTier:           output.TierZeroConfig,
			expectedNameResolution: validateresult.CheckSkipped,
			expectCapabilityWarn:   true,
		},
		{
			name: "schemas present and table resolves",
			args: map[string]any{
				"sql":     "SELECT id FROM users",
				"schemas": []any{map[string]any{"sql": usersDDL}},
			},
			expectedValid:          true,
			expectedTier:           output.TierSchemaFile,
			expectedNameResolution: validateresult.CheckOK,
		},
		{
			name: "schemas present and table missing yields 42P01",
			args: map[string]any{
				"sql":     "SELECT id FROM nope",
				"schemas": []any{map[string]any{"sql": usersDDL}},
			},
			expectedEnvErrs: true,
			expectedCode:    "42P01",
			expectedTier:    output.TierSchemaFile,
		},
		{
			name: "malformed schema DDL yields envelope parse error",
			args: map[string]any{
				"sql":     "SELECT 1",
				"schemas": []any{map[string]any{"sql": "CREATE TABLEE bad (id INT)"}},
			},
			expectedEnvErrs:     true,
			expectedCode:        "42601",
			expectedTier:        output.TierSchemaFile,
			expectNoFailureData: true,
		},
		{
			name: "duplicate-table schema surfaces schema_warning alongside successful resolution",
			args: map[string]any{
				"sql": "SELECT id FROM users",
				"schemas": []any{
					map[string]any{"sql": usersDDL},
					map[string]any{"sql": usersDDL},
				},
			},
			expectedValid:          true,
			expectedTier:           output.TierSchemaFile,
			expectedNameResolution: validateresult.CheckOK,
			expectSchemaWarning:    true,
		},
		{
			name: "empty schemas array still skips name resolution",
			args: map[string]any{
				"sql":     "SELECT 1",
				"schemas": []any{},
			},
			expectedValid:          true,
			expectedTier:           output.TierZeroConfig,
			expectedNameResolution: validateresult.CheckSkipped,
			expectCapabilityWarn:   true,
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
		{
			name: "malformed schemas yields tool error",
			args: map[string]any{
				"sql":     "SELECT 1",
				"schemas": "not-an-array",
			},
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
			require.Equal(t, tc.expectedTier, env.Tier)
			require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)

			if tc.expectedEnvErrs {
				require.NotEmpty(t, env.Errors)
				if tc.expectedCode != "" {
					require.Equal(t, tc.expectedCode, env.Errors[0].Code)
				}
				if tc.expectNoFailureData {
					require.Nil(t, env.Data)
					return
				}
				var failResult validateresult.Result
				require.NoError(t, json.Unmarshal(env.Data, &failResult))
				require.False(t, failResult.Valid)
				return
			}

			require.NotNil(t, env.Data)

			var result validateresult.Result
			require.NoError(t, json.Unmarshal(env.Data, &result))
			require.Equal(t, tc.expectedValid, result.Valid)
			require.Equal(t, validateresult.CheckOK, result.Checks.Syntax)
			require.Equal(t, validateresult.CheckOK, result.Checks.TypeCheck)
			require.Equal(t, tc.expectedNameResolution, result.Checks.NameResolution)

			switch {
			case tc.expectCapabilityWarn:
				require.Len(t, env.Errors, 1)
				require.Equal(t, validateresult.CapabilityRequiredCode, env.Errors[0].Code)
				require.Equal(t, output.SeverityWarning, env.Errors[0].Severity)
				require.Equal(t, validateresult.CapabilityNameResolution, env.Errors[0].Context["capability"])
			case tc.expectSchemaWarning:
				require.Len(t, env.Errors, 1)
				require.Equal(t, "schema_warning", env.Errors[0].Code)
				require.Equal(t, output.SeverityWarning, env.Errors[0].Severity)
			default:
				require.Empty(t, env.Errors)
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

func TestSummarizeSQLHandler(t *testing.T) {
	tests := []struct {
		name                 string
		args                 map[string]any
		expectedToolErr      bool
		expectedEnvErrs      bool
		expectedCode         string
		expectedSummaryCount int
		expectedOperation    summarize.Operation
		expectedTables       []string
		expectedPredicates   []string
	}{
		{
			name:                 "DELETE with WHERE",
			args:                 map[string]any{"sql": "DELETE FROM orders WHERE status='x'"},
			expectedSummaryCount: 1,
			expectedOperation:    summarize.OpDelete,
			expectedTables:       []string{"orders"},
			expectedPredicates:   []string{"status = 'x'"},
		},
		{
			name:                 "multiple statements",
			args:                 map[string]any{"sql": "SELECT 1; UPDATE t SET x=1 WHERE id=1"},
			expectedSummaryCount: 2,
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
			handler := SummarizeSQLHandler(testParserVersion, "" /* defaultTargetVersion */)
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

			var summaries []summarize.Summary
			require.NoError(t, json.Unmarshal(env.Data, &summaries))
			require.Len(t, summaries, tc.expectedSummaryCount)

			if tc.expectedOperation != "" {
				require.Equal(t, tc.expectedOperation, summaries[0].Operation)
			}
			if tc.expectedTables != nil {
				require.Equal(t, tc.expectedTables, summaries[0].Tables)
			}
			if tc.expectedPredicates != nil {
				require.Equal(t, tc.expectedPredicates, summaries[0].Predicates)
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

// TestParseSQLHandler_VersionWarning exercises the inspector wired
// into the MCP parse_sql tool: a per-call target_version that
// predates the seeded plpgsql_function_body Introduced version
// produces a feature_not_yet_introduced WARNING in the envelope
// while the data payload (classified statements) still populates.
//
// Mirrors TestParseCmdVersionWarning_PLpgSQL on the CLI surface so a
// regression in either path is caught locally.
func TestParseSQLHandler_VersionWarning(t *testing.T) {
	const plpgsqlSQL = `CREATE FUNCTION f() RETURNS INT LANGUAGE PLpgSQL AS $$ BEGIN RETURN 1; END $$`

	tests := []struct {
		name           string
		targetVersion  string
		expectFeatWarn bool
	}{
		{name: "before introduced", targetVersion: "23.2", expectFeatWarn: true},
		{name: "at introduced", targetVersion: "24.1", expectFeatWarn: false},
		{name: "no target version skips", targetVersion: "", expectFeatWarn: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := ParseSQLHandler(testParserVersion, "" /* defaultTargetVersion */)
			args := map[string]any{"sql": plpgsqlSQL}
			if tc.targetVersion != "" {
				args["target_version"] = tc.targetVersion
			}
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = args

			res, err := handler(context.Background(), req)
			require.NoError(t, err)
			env := requireEnvelope(t, res)
			require.NotEmpty(t, env.Data, "data payload must populate even with warnings")

			featWarn := findEnvErrorByCode(env.Errors, output.CodeFeatureNotYetIntroduced)
			if !tc.expectFeatWarn {
				require.Nilf(t, featWarn, "unexpected feature warning: %+v", featWarn)
				return
			}
			require.NotNilf(t, featWarn, "expected feature_not_yet_introduced in %+v", env.Errors)
			require.Equal(t, output.SeverityWarning, featWarn.Severity)
			require.Equal(t, "plpgsql_function_body", featWarn.Context["feature_tag"])
			require.Equal(t, tc.targetVersion, featWarn.Context["target"])
		})
	}
}

// TestValidateSQLHandler_VersionWarning is the MCP-side mirror of the
// issue #84 demo: per-call target_version=23.2 with PL/pgSQL emits
// the feature warning while keeping valid: true. Coverage at this
// layer ensures CLI and MCP stay in sync as the inspector grows.
func TestValidateSQLHandler_VersionWarning(t *testing.T) {
	const plpgsqlSQL = `CREATE FUNCTION f() RETURNS INT LANGUAGE PLpgSQL AS $$ BEGIN RETURN 1; END $$`

	handler := ValidateSQLHandler(testParserVersion, "" /* defaultTargetVersion */)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"sql":            plpgsqlSQL,
		"target_version": "23.2",
	}

	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	env := requireEnvelope(t, res)

	featWarn := findEnvErrorByCode(env.Errors, output.CodeFeatureNotYetIntroduced)
	require.NotNilf(t, featWarn, "expected feature warning in %+v", env.Errors)
	require.Equal(t, "plpgsql_function_body", featWarn.Context["feature_tag"])

	var result validateresult.Result
	require.NoError(t, json.Unmarshal(env.Data, &result))
	require.True(t, result.Valid, "warnings are advisory; verdict must remain valid")
}

// findEnvErrorByCode returns the first envelope Error whose Code
// matches code, or nil. Tests use it instead of indexing into the
// Errors slice so that an unrelated warning (e.g.
// target_version_mismatch from the parser-version skew) sitting at
// position 0 doesn't shift the assertion target.
func findEnvErrorByCode(errs []output.Error, code string) *output.Error {
	for i := range errs {
		if errs[i].Code == code {
			return &errs[i]
		}
	}
	return nil
}

// TestParseSQLHandler_ParseErrorPreservesPriorWarnings pins that the
// parse handler appends (rather than overwrites) on parse failure: a
// pre-stamped target_version_mismatch warning from baseEnvelope must
// survive into the response. Without this, agents lose the version-
// skew signal exactly when their SQL is malformed.
func TestParseSQLHandler_ParseErrorPreservesPriorWarnings(t *testing.T) {
	handler := ParseSQLHandler(testParserVersion, "" /* defaultTargetVersion */)
	req := mcpgo.CallToolRequest{}
	// target_version with a different MAJOR.MINOR than testParserVersion
	// (v0.26.2) triggers VersionMismatchWarning; SELECTT 1 then forces
	// a parse error.
	req.Params.Arguments = map[string]any{
		"sql":            "SELECTT 1",
		"target_version": "25.4",
	}

	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	env := requireEnvelope(t, res)

	require.NotNilf(t, findEnvErrorByCode(env.Errors, output.CodeTargetVersionMismatch),
		"target_version_mismatch warning must survive parse failure: %+v", env.Errors)
	require.NotNilf(t, findEnvErrorByCode(env.Errors, "42601"),
		"parse error must still be present: %+v", env.Errors)
}

// TestValidateSQLHandler_ParseErrorPreservesPriorWarnings is the
// validate-side mirror — the same append-not-overwrite contract on
// the validate handler's parse-error branch.
func TestValidateSQLHandler_ParseErrorPreservesPriorWarnings(t *testing.T) {
	handler := ValidateSQLHandler(testParserVersion, "" /* defaultTargetVersion */)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"sql":            "SELECTT 1",
		"target_version": "25.4",
	}

	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	env := requireEnvelope(t, res)

	require.NotNilf(t, findEnvErrorByCode(env.Errors, output.CodeTargetVersionMismatch),
		"target_version_mismatch warning must survive parse failure: %+v", env.Errors)
	require.NotNilf(t, findEnvErrorByCode(env.Errors, "42601"),
		"parse error must still be present: %+v", env.Errors)
}
