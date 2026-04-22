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

// TestValidateCmdSuggestionsJSON verifies that name-resolution errors
// surface structured suggestions through the JSON envelope. Anchors
// the end-to-end wiring of #15: semcheck attaches Suggestions, the
// envelope serializes the field with the expected replacement and
// byte range.
func TestValidateCmdSuggestionsJSON(t *testing.T) {
	dir := t.TempDir()
	schema := writeFile(t, dir, "schema.sql",
		"CREATE TABLE users (id INT PRIMARY KEY, name TEXT);\n")

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"validate",
		"--output", "json",
		"--schema", schema,
		"-e", "SELECT nme FROM users",
	})

	require.ErrorIs(t, root.Execute(), output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.Equal(t, "42703", env.Errors[0].Code)
	require.NotEmpty(t, env.Errors[0].Suggestions)
	first := env.Errors[0].Suggestions[0]
	require.Equal(t, "name", first.Replacement)
	require.Equal(t, 7, first.Range.Start)
	require.Equal(t, 10, first.Range.End)
	require.Equal(t, "levenshtein_distance_1", first.Reason)
}

// TestValidateCmdSuggestionsText verifies that text-mode rendering
// prints "did you mean: X" lines under errors. Locks in the renderer
// tweak so a future refactor cannot silently drop the field.
func TestValidateCmdSuggestionsText(t *testing.T) {
	dir := t.TempDir()
	schema := writeFile(t, dir, "schema.sql",
		"CREATE TABLE users (id INT PRIMARY KEY, name TEXT);\n")

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"validate",
		"--schema", schema,
		"-e", "SELECT nme FROM users",
	})

	require.ErrorIs(t, root.Execute(), output.ErrRendered)
	// Lock both the replacement and the percentage format. A future
	// refactor that drops the "(NN% confidence)" suffix or shifts
	// rounding (e.g. removing the +0.5 bias) would silently change
	// every text-mode user's output without this assertion.
	require.Contains(t, stdout.String(), "did you mean: name (75% confidence)")
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

// writeFile is a small test helper that creates a file under dir
// (creating parents as needed) and returns the full path.
func writeFile(t *testing.T, dir, rel, contents string) string {
	t.Helper()
	full := filepath.Join(dir, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(contents), 0o644))
	return full
}

// setupValidateConfigProject lays down a tiny project with one
// schema, one valid query, one query that fails to parse, and a
// crdb-sql.yaml that ties them together. Returns the directory and
// the absolute paths of the two query files for assertion-side use.
func setupValidateConfigProject(t *testing.T) (dir, goodQuery, badQuery string) {
	t.Helper()
	dir = t.TempDir()
	writeFile(t, dir, "schema/users.sql",
		"CREATE TABLE users (id INT PRIMARY KEY, name STRING);\n")
	goodQuery = writeFile(t, dir, "queries/q1.sql", "SELECT * FROM users;\n")
	badQuery = writeFile(t, dir, "queries/sub/q2_bad.sql", "SELECT FROM\n")
	writeFile(t, dir, "crdb-sql.yaml", `version: 1
sql:
  - schema: ["schema/*.sql"]
    queries: ["queries/**/*.sql"]
`)
	return dir, goodQuery, badQuery
}

// TestValidateCmdConfigJSON verifies the YAML-driven path: with a
// config and no -e/file arg, validate iterates every matched query
// file, attaches the file path to each error's Context, and exits
// non-zero overall when any file is invalid.
func TestValidateCmdConfigJSON(t *testing.T) {
	dir, goodQuery, badQuery := setupValidateConfigProject(t)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"validate",
		"--output", "json",
		"--config", filepath.Join(dir, "crdb-sql.yaml"),
	})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered, "any failed file must surface as ErrRendered")

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, output.TierSchemaFile, env.Tier)
	require.Len(t, env.Errors, 1, "exactly the bad query should produce an error")

	gotErr := env.Errors[0]
	require.Equal(t, "42601", gotErr.Code)
	require.Equal(t, badQuery, gotErr.Context["file"])

	var payload struct {
		Files []struct {
			File       string `json:"file"`
			Valid      bool   `json:"valid"`
			ErrorCount int    `json:"error_count"`
		} `json:"files"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &payload))
	require.Len(t, payload.Files, 2)

	byFile := map[string]bool{}
	for _, f := range payload.Files {
		byFile[f.File] = f.Valid
	}
	require.True(t, byFile[goodQuery], "good query should be valid")
	require.False(t, byFile[badQuery], "bad query should be invalid")
}

// TestValidateCmdConfigText verifies that text-mode output for the
// config path lists each file with a status line.
func TestValidateCmdConfigText(t *testing.T) {
	dir, goodQuery, badQuery := setupValidateConfigProject(t)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"validate",
		"--config", filepath.Join(dir, "crdb-sql.yaml"),
	})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	got := stdout.String()
	require.Contains(t, got, goodQuery+": valid")
	require.Contains(t, got, badQuery+":")
	require.Contains(t, got, "syntax error")
	require.Contains(t, got, "42601")
}

// TestValidateCmdConfigAllValid verifies that a config whose query
// files all parse cleanly exits zero (no ErrRendered) and reports no
// errors.
func TestValidateCmdConfigAllValid(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "schema/users.sql",
		"CREATE TABLE users (id INT PRIMARY KEY);\n")
	good1 := writeFile(t, dir, "queries/q1.sql", "SELECT * FROM users;\n")
	good2 := writeFile(t, dir, "queries/q2.sql", "SELECT 1;\n")
	writeFile(t, dir, "crdb-sql.yaml", `version: 1
sql:
  - schema: ["schema/*.sql"]
    queries: ["queries/*.sql"]
`)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"validate",
		"--output", "json",
		"--config", filepath.Join(dir, "crdb-sql.yaml"),
	})

	require.NoError(t, root.Execute())

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Empty(t, env.Errors)

	var payload struct {
		Files []struct {
			File  string `json:"file"`
			Valid bool   `json:"valid"`
		} `json:"files"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &payload))
	require.Len(t, payload.Files, 2)
	for _, f := range payload.Files {
		require.True(t, f.Valid, "file %s should be valid", f.File)
	}
	require.Contains(t, []string{payload.Files[0].File, payload.Files[1].File}, good1)
	require.Contains(t, []string{payload.Files[0].File, payload.Files[1].File}, good2)
}

// TestValidateCmdCLIOverridesConfig verifies that an explicit -e
// short-circuits the config path even when a config is loaded — the
// per-user precedence rule.
func TestValidateCmdCLIOverridesConfig(t *testing.T) {
	dir, _, _ := setupValidateConfigProject(t)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"validate",
		"--config", filepath.Join(dir, "crdb-sql.yaml"),
		"-e", "SELECT 1",
	})

	require.NoError(t, root.Execute())
	require.Equal(t, validTextNoSchema, stdout.String(),
		"explicit -e must short-circuit the config path and run the no-schema text path")
}

// TestValidateCmdConfigCLISchemaWins verifies that an explicit --schema
// also short-circuits the config path: the user is asking for a one-off
// validation against a specific schema, not the project default.
func TestValidateCmdConfigCLISchemaWins(t *testing.T) {
	dir, _, _ := setupValidateConfigProject(t)
	otherSchema := writeUsersSchema(t)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"validate",
		"--config", filepath.Join(dir, "crdb-sql.yaml"),
		"--schema", otherSchema,
		"-e", "SELECT * FROM users",
	})

	require.NoError(t, root.Execute())
	require.Equal(t, "Valid.\n", stdout.String(),
		"explicit --schema must use the single-input flow with name resolution")
}

// TestValidateCmdConfigUnknownTable verifies that the YAML config path
// runs name resolution against the loaded schema and surfaces unknown-
// table errors per file (proving the catalog is actually consulted, not
// just loaded).
func TestValidateCmdConfigUnknownTable(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "schema/users.sql",
		"CREATE TABLE users (id INT PRIMARY KEY);\n")
	bad := writeFile(t, dir, "queries/q.sql", "SELECT * FROM nonexistent;\n")
	writeFile(t, dir, "crdb-sql.yaml", `version: 1
sql:
  - schema: ["schema/*.sql"]
    queries: ["queries/*.sql"]
`)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"validate",
		"--output", "json",
		"--config", filepath.Join(dir, "crdb-sql.yaml"),
	})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.Equal(t, "42P01", env.Errors[0].Code)
	require.Equal(t, bad, env.Errors[0].Context["file"])
}

// TestValidateCmdConfigMissingFileFails verifies that pointing
// --config at a non-existent path is a hard error rather than a
// silent fall-through (Discover tolerates absence; explicit Load does
// not).
func TestValidateCmdConfigMissingFileFails(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"validate",
		"--config", filepath.Join(t.TempDir(), "does-not-exist.yaml"),
	})

	err := root.Execute()
	require.Error(t, err)
}

// TestValidateCmdConfigMultiPair verifies that two pairs each get their
// own catalog: a query that references a table from one pair must not
// resolve against the other pair's schema. This guards against a
// regression where the catalog construction is hoisted out of the
// per-pair loop.
func TestValidateCmdConfigMultiPair(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "prod/schema.sql",
		"CREATE TABLE users (id INT PRIMARY KEY);\n")
	writeFile(t, dir, "test/schema.sql",
		"CREATE TABLE fixtures (id INT PRIMARY KEY);\n")
	prodGood := writeFile(t, dir, "prod/queries/q.sql", "SELECT * FROM users;\n")
	testGood := writeFile(t, dir, "test/queries/q.sql", "SELECT * FROM fixtures;\n")
	prodBad := writeFile(t, dir, "prod/queries/cross.sql", "SELECT * FROM fixtures;\n")
	writeFile(t, dir, "crdb-sql.yaml", `version: 1
sql:
  - schema: ["prod/schema.sql"]
    queries: ["prod/queries/*.sql"]
  - schema: ["test/schema.sql"]
    queries: ["test/queries/*.sql"]
`)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"validate",
		"--output", "json",
		"--config", filepath.Join(dir, "crdb-sql.yaml"),
	})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered, "prod/cross.sql references the test pair's table; pair isolation must surface this")

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Len(t, env.Errors, 1)
	require.Equal(t, "42P01", env.Errors[0].Code)
	require.Equal(t, prodBad, env.Errors[0].Context["file"])

	var payload struct {
		Files []struct {
			File  string `json:"file"`
			Valid bool   `json:"valid"`
		} `json:"files"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &payload))
	require.Len(t, payload.Files, 3)

	byFile := map[string]bool{}
	for _, f := range payload.Files {
		byFile[f.File] = f.Valid
	}
	require.True(t, byFile[prodGood], "prod query against prod schema must resolve")
	require.True(t, byFile[testGood], "test query against test schema must resolve")
	require.False(t, byFile[prodBad], "prod query against test-only table must fail")
}

// TestValidateCmdConfigBadSchemaFails verifies that a DDL parse
// failure in a config-listed schema file aborts validation entirely
// (the catalog is a config-level prerequisite, not a per-file
// concern).
func TestValidateCmdConfigBadSchemaFails(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "schema/broken.sql", "CREATE TABLE bad (")
	writeFile(t, dir, "queries/q.sql", "SELECT 1;")
	writeFile(t, dir, "crdb-sql.yaml", `version: 1
sql:
  - schema: ["schema/*.sql"]
    queries: ["queries/*.sql"]
`)

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"validate",
		"--output", "json",
		"--config", filepath.Join(dir, "crdb-sql.yaml"),
	})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.NotEmpty(t, env.Errors)
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

// TestValidateCmdSchemaUnknownColumn verifies that --schema enables
// column-name resolution and that an unknown column produces a 42703
// envelope error with the table's columns in available_columns. This
// is the end-to-end demo from issue #14.
func TestValidateCmdSchemaUnknownColumn(t *testing.T) {
	dir := t.TempDir()
	schema := filepath.Join(dir, "schema.sql")
	require.NoError(t, os.WriteFile(schema,
		[]byte("CREATE TABLE users (id INT PRIMARY KEY, name TEXT);"), 0644))

	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"validate", "--output", "json",
		"--schema", schema, "-e", "SELECT nme FROM users"})

	err := root.Execute()
	require.ErrorIs(t, err, output.ErrRendered)

	var env output.Envelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &env))
	require.Equal(t, output.TierSchemaFile, env.Tier)
	require.Len(t, env.Errors, 1)
	require.Nil(t, env.Data)

	diagErr := env.Errors[0]
	require.Equal(t, "42703", diagErr.Code)
	require.Equal(t, "unknown_column", diagErr.Category)
	require.Contains(t, diagErr.Message, "nme")
	require.NotNil(t, diagErr.Position)
	require.Equal(t, 1, diagErr.Position.Line)
	require.Equal(t, 8, diagErr.Position.Column)
	avail, ok := diagErr.Context["available_columns"].([]any)
	require.True(t, ok, "available_columns must be a JSON array")
	require.Equal(t, []any{"id", "name"}, avail)
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
