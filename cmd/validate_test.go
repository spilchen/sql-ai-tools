// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/spilchen/sql-ai-tools/internal/output"
)

// validTextNoSchema is the text-mode stdout produced by a successful
// validate run when --schema is not supplied: the success line plus the
// capability_required note. Centralised so behavior changes only need
// to update one place.
const validTextNoSchema = "Valid.\nnote: name resolution skipped (pass --schema to enable)\n"

// writeUsersSchema writes a minimal "users" schema to a temp file and
// returns its path. Used by tests that exercise the --schema code path.
func writeUsersSchema(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "schema.sql")
	require.NoError(t, os.WriteFile(path,
		[]byte("CREATE TABLE users (id INT PRIMARY KEY);"), 0644))
	return path
}

// TestValidateCmdTextSuccess verifies that valid SQL in text mode
// prints "Valid." plus the capability_required note (because no
// --schema was provided).
func TestValidateCmdTextSuccess(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("SELECT 1"))
	root.SetArgs([]string{"validate"})

	require.NoError(t, root.Execute())
	require.Equal(t, validTextNoSchema, stdout.String())
}

// TestValidateCmdJSONSuccess verifies the JSON envelope shape on the
// no-schema success path: tier=zero_config, a single capability_required
// warning entry, and a checks block recording that name resolution was
// skipped.
func TestValidateCmdJSONSuccess(t *testing.T) {
	root := newRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader("SELECT 1"))
	root.SetArgs([]string{"validate", "--output", "json"})

	require.NoError(t, root.Execute())
	require.Empty(t, stderr.String(), "JSON mode must not write to stderr")

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, output.TierZeroConfig, env.Tier)
	require.Equal(t, output.ConnectionDisconnected, env.ConnectionStatus)
	require.NotEmpty(t, env.ParserVersion)

	require.Len(t, env.Errors, 1)
	require.Equal(t, "capability_required", env.Errors[0].Code)
	require.Equal(t, output.SeverityWarning, env.Errors[0].Severity)
	require.Equal(t, "capability_required", env.Errors[0].Category)
	require.Equal(t, "name_resolution", env.Errors[0].Context["capability"])

	var payload struct {
		Valid  bool `json:"valid"`
		Checks struct {
			Syntax         string `json:"syntax"`
			TypeCheck      string `json:"type_check"`
			NameResolution string `json:"name_resolution"`
		} `json:"checks"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &payload))
	require.True(t, payload.Valid)
	require.Equal(t, "ok", payload.Checks.Syntax)
	require.Equal(t, "ok", payload.Checks.TypeCheck)
	require.Equal(t, "skipped", payload.Checks.NameResolution)
}

// TestValidateCmdTextError verifies that invalid SQL in text mode
// outputs a formatted error line with position and SQLSTATE code.
func TestValidateCmdTextError(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("SELECT FROM"))
	root.SetArgs([]string{"validate"})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	got := stdout.String()
	require.Contains(t, got, "1:12:")
	require.Contains(t, got, "syntax error")
	require.Contains(t, got, "42601")
}

// TestValidateCmdJSONError verifies that invalid SQL in JSON mode
// produces an envelope with structured errors and nil data.
func TestValidateCmdJSONError(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("SELECT FROM"))
	root.SetArgs([]string{"validate", "--output", "json"})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.Nil(t, env.Data)

	diagErr := env.Errors[0]
	require.Equal(t, "42601", diagErr.Code)
	require.Equal(t, output.SeverityError, diagErr.Severity)
	require.Equal(t, "syntax_error", diagErr.Category)
	require.Contains(t, diagErr.Message, "syntax error")
	require.NotNil(t, diagErr.Position)
	require.Equal(t, 1, diagErr.Position.Line)
	require.Equal(t, 12, diagErr.Position.Column)
	require.Equal(t, 11, diagErr.Position.ByteOffset)
}

// TestValidateCmdExprFlag verifies the -e flag path.
func TestValidateCmdExprFlag(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"validate", "-e", "SELECT 1"})

	require.NoError(t, root.Execute())
	require.Equal(t, validTextNoSchema, stdout.String())
}

// TestValidateCmdFileArg verifies reading SQL from a file argument.
func TestValidateCmdFileArg(t *testing.T) {
	dir := t.TempDir()
	sqlFile := filepath.Join(dir, "input.sql")
	require.NoError(t, os.WriteFile(sqlFile, []byte("SELECT 1"), 0644))

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"validate", sqlFile})

	require.NoError(t, root.Execute())
	require.Equal(t, validTextNoSchema, stdout.String())
}

// TestValidateCmdEmptyInput verifies that empty stdin produces an error.
func TestValidateCmdEmptyInput(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader(""))
	root.SetArgs([]string{"validate"})

	require.Error(t, root.Execute())
}

// TestValidateCmdConflictingInput verifies that -e and a file arg
// together produce an error.
func TestValidateCmdConflictingInput(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"validate", "-e", "SELECT 1", "somefile.sql"})

	err := root.Execute()
	require.Error(t, err)
	require.ErrorContains(t, err, "cannot use -e flag and file argument together")
}

// TestValidateCmdMultiStatementSuccess verifies that multiple valid
// statements are all accepted.
func TestValidateCmdMultiStatementSuccess(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("SELECT 1; SELECT 2"))
	root.SetArgs([]string{"validate"})

	require.NoError(t, root.Execute())
	require.Equal(t, validTextNoSchema, stdout.String())
}

// TestValidateCmdTypeErrorText verifies that a type mismatch in text
// mode outputs an error line with the SQLSTATE code.
func TestValidateCmdTypeErrorText(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"validate", "-e", "SELECT 1 + 'hello'"})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	got := stdout.String()
	require.Contains(t, got, "unsupported binary operator")
}

// TestValidateCmdTypeErrorJSON verifies that a type mismatch in JSON
// mode produces an envelope with a structured error and nil data.
func TestValidateCmdTypeErrorJSON(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"validate", "--output", "json", "-e", "SELECT 1 + 'hello'"})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.Nil(t, env.Data)

	diagErr := env.Errors[0]
	require.NotEmpty(t, diagErr.Code)
	require.Equal(t, output.SeverityError, diagErr.Severity)
	require.Contains(t, diagErr.Message, "unsupported binary operator")
	require.NotNil(t, diagErr.Position)
	require.Equal(t, 1, diagErr.Position.Line)
}

// TestValidateCmdColumnRefNoTypeError verifies that SQL with column
// references does not produce false-positive type errors.
func TestValidateCmdColumnRefNoTypeError(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("SELECT a + 1 FROM t"))
	root.SetArgs([]string{"validate"})

	require.NoError(t, root.Execute())
	require.Equal(t, validTextNoSchema, stdout.String())
}

// TestValidateCmdMultiStatementError verifies that an error in a later
// statement reports the correct position relative to the full input.
func TestValidateCmdMultiStatementError(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader("SELECT 1;\nSELECT FROM"))
	root.SetArgs([]string{"validate", "--output", "json"})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)

	pos := env.Errors[0].Position
	require.NotNil(t, pos)
	require.Equal(t, 2, pos.Line)
	require.Equal(t, 12, pos.Column)
	require.Equal(t, 21, pos.ByteOffset)
}

// TestValidateCmdSchemaUnknownTable verifies that --schema enables name
// resolution and that an unknown table produces a 42P01 envelope error
// with the catalog's tables in available_tables.
func TestValidateCmdSchemaUnknownTable(t *testing.T) {
	schema := writeUsersSchema(t)
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"validate", "--output", "json",
		"--schema", schema, "-e", "SELECT * FROM usrs"})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, output.TierSchemaFile, env.Tier)
	require.Len(t, env.Errors, 1)
	require.Nil(t, env.Data)

	diagErr := env.Errors[0]
	require.Equal(t, "42P01", diagErr.Code)
	require.Equal(t, "unknown_table", diagErr.Category)
	require.Contains(t, diagErr.Message, "usrs")
	avail, ok := diagErr.Context["available_tables"].([]any)
	require.True(t, ok, "available_tables must be a JSON array")
	require.Equal(t, []any{"users"}, avail)
}

// TestValidateCmdSchemaKnownTable verifies the success path with
// --schema: tier=schema_file, no errors, name_resolution=ok.
func TestValidateCmdSchemaKnownTable(t *testing.T) {
	schema := writeUsersSchema(t)
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"validate", "--output", "json",
		"--schema", schema, "-e", "SELECT * FROM users"})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, output.TierSchemaFile, env.Tier)
	require.Empty(t, env.Errors)

	var payload struct {
		Valid  bool `json:"valid"`
		Checks struct {
			NameResolution string `json:"name_resolution"`
		} `json:"checks"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &payload))
	require.True(t, payload.Valid)
	require.Equal(t, "ok", payload.Checks.NameResolution)
}

// TestValidateCmdSchemaUnknownTableText verifies that unknown-table
// errors render in text mode with line/column and the SQLSTATE code.
func TestValidateCmdSchemaUnknownTableText(t *testing.T) {
	schema := writeUsersSchema(t)
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"validate",
		"--schema", schema, "-e", "SELECT * FROM usrs"})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	got := stdout.String()
	require.Contains(t, got, "1:15:")
	require.Contains(t, got, `"usrs"`)
	require.Contains(t, got, "42P01")
}

// TestValidateCmdSchemaParseError verifies that a malformed schema
// file surfaces the parser's SQLSTATE (42601) rather than the generic
// internal_error code.
func TestValidateCmdSchemaParseError(t *testing.T) {
	dir := t.TempDir()
	schema := filepath.Join(dir, "bad.sql")
	require.NoError(t, os.WriteFile(schema,
		[]byte("CREATE TABLE FROM"), 0644))

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"validate", "--output", "json",
		"--schema", schema, "-e", "SELECT 1"})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.Equal(t, "42601", env.Errors[0].Code)
	require.Equal(t, "syntax_error", env.Errors[0].Category)
	require.Contains(t, env.Errors[0].Message, "bad.sql")
}

// TestValidateCmdSchemaMissingFile verifies that an unreadable schema
// path is reported with the dedicated schema_load_error code rather
// than the generic internal_error code.
func TestValidateCmdSchemaMissingFile(t *testing.T) {
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"validate", "--output", "json",
		"--schema", "/nonexistent/schema.sql", "-e", "SELECT 1"})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.Equal(t, "schema_load_error", env.Errors[0].Code)
	require.Contains(t, env.Errors[0].Message, "/nonexistent/schema.sql")
}

// TestValidateCmdSchemaTextSuccess verifies that text mode with
// --schema and a known table prints "Valid." with no skipped-note.
func TestValidateCmdSchemaTextSuccess(t *testing.T) {
	schema := writeUsersSchema(t)
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"validate",
		"--schema", schema, "-e", "SELECT * FROM users"})

	require.NoError(t, root.Execute())
	require.Equal(t, "Valid.\n", stdout.String())
}
