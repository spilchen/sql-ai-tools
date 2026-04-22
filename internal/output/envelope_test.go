// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEnvelopeMarshal pins the on-the-wire JSON layout. Downstream
// agents key off these field names; an accidental rename or omitempty
// change must fail this test rather than ship as a silent breaking
// change.
func TestEnvelopeMarshal(t *testing.T) {
	tests := []struct {
		name         string
		envelope     Envelope
		expectedJSON string
	}{
		{
			name: "minimal version-style envelope omits tier and errors",
			envelope: Envelope{
				ParserVersion:    "v0.26.2",
				ConnectionStatus: ConnectionDisconnected,
				Data:             json.RawMessage(`{"binary_version":"dev"}`),
			},
			expectedJSON: `{
  "parser_version": "v0.26.2",
  "connection_status": "disconnected",
  "data": {
    "binary_version": "dev"
  }
}`,
		},
		{
			name: "tier emitted when set",
			envelope: Envelope{
				Tier:             TierSchemaFile,
				ParserVersion:    "v0.26.2",
				ConnectionStatus: ConnectionDisconnected,
			},
			expectedJSON: `{
  "tier": "schema_file",
  "parser_version": "v0.26.2",
  "connection_status": "disconnected"
}`,
		},
		{
			name: "target_version emitted when set",
			envelope: Envelope{
				Tier:             TierZeroConfig,
				ParserVersion:    "v0.26.2",
				TargetVersion:    "25.4.0",
				ConnectionStatus: ConnectionDisconnected,
			},
			expectedJSON: `{
  "tier": "zero_config",
  "parser_version": "v0.26.2",
  "target_version": "25.4.0",
  "connection_status": "disconnected"
}`,
		},
		{
			name: "errors-only envelope",
			envelope: Envelope{
				ParserVersion:    "v0.26.2",
				ConnectionStatus: ConnectionDisconnected,
				Errors: []Error{{
					Code:     "42703",
					Severity: SeverityError,
					Message:  `column "nme" does not exist`,
					Position: &Position{Line: 1, Column: 8, ByteOffset: 7},
					Category: "unknown_column",
					Suggestions: []Suggestion{{
						Replacement: "name",
						Range:       Range{Start: 7, End: 10},
						Confidence:  0.9,
						Reason:      "levenshtein_distance_1",
					}},
				}},
			},
			expectedJSON: `{
  "parser_version": "v0.26.2",
  "connection_status": "disconnected",
  "errors": [
    {
      "code": "42703",
      "severity": "ERROR",
      "message": "column \"nme\" does not exist",
      "position": {
        "line": 1,
        "column": 8,
        "byte_offset": 7
      },
      "category": "unknown_column",
      "suggestions": [
        {
          "replacement": "name",
          "range": {
            "start": 7,
            "end": 10
          },
          "confidence": 0.9,
          "reason": "levenshtein_distance_1"
        }
      ]
    }
  ]
}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.MarshalIndent(tc.envelope, "", "  ")
			require.NoError(t, err)
			require.Equal(t, tc.expectedJSON, string(got))
		})
	}
}

// TestParseFormat covers the validation contract: only "text" and
// "json" pass; anything else returns an error naming both valid
// choices.
func TestParseFormat(t *testing.T) {
	tests := []struct {
		name           string
		input          string
		expected       Format
		expectedErrSub string
	}{
		{name: "text", input: "text", expected: FormatText},
		{name: "json", input: "json", expected: FormatJSON},
		{name: "case sensitive: TEXT rejected", input: "TEXT", expectedErrSub: `invalid --output "TEXT"`},
		{name: "unknown format rejected", input: "xml", expectedErrSub: `"text"`},
		{name: "empty rejected", input: "", expectedErrSub: `"json"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseFormat(tc.input)
			if tc.expectedErrSub != "" {
				require.ErrorContains(t, err, tc.expectedErrSub)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expected, got)
		})
	}
}

// TestRendererText delegates to the supplied text closure and ignores
// the envelope.
func TestRendererText(t *testing.T) {
	var buf bytes.Buffer
	r := Renderer{Format: FormatText, Out: &buf}
	err := r.Render(Envelope{ParserVersion: "v0.26.2"}, func(w io.Writer) error {
		_, err := w.Write([]byte("hello\n"))
		return err
	})
	require.NoError(t, err)
	require.Equal(t, "hello\n", buf.String())
}

// fakeWriter is a stub io.Writer that returns a fixed error from Write.
// Used to drive the EPIPE-suppression and write-error branches without
// touching real OS pipes (which would be racy under `go test`).
type fakeWriter struct {
	err error
}

func (w fakeWriter) Write(_ []byte) (int, error) { return 0, w.err }

// TestRendererSuppressesEPIPE pins the contract that EPIPE from the
// downstream writer is silently dropped (downstream consumer closed
// stdout early, e.g. `crdb-sql foo | head -n1`) but unrelated write
// errors propagate. Both formats share the same suppression site, so
// both are exercised here.
func TestRendererSuppressesEPIPE(t *testing.T) {
	otherErr := errors.New("disk full")

	tests := []struct {
		name         string
		writeErr     error
		expectedErr  error
		expectedNoOp bool
	}{
		{name: "no error", writeErr: nil, expectedNoOp: true},
		{name: "bare EPIPE suppressed", writeErr: syscall.EPIPE, expectedNoOp: true},
		{name: "wrapped EPIPE suppressed", writeErr: fmt.Errorf("write: %w", syscall.EPIPE), expectedNoOp: true},
		{name: "unrelated write error propagates", writeErr: otherErr, expectedErr: otherErr},
	}

	for _, tc := range tests {
		t.Run(tc.name+"/json", func(t *testing.T) {
			r := Renderer{Format: FormatJSON, Out: fakeWriter{err: tc.writeErr}}
			err := r.Render(Envelope{ParserVersion: "v0.26.2"}, nil)
			if tc.expectedNoOp {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.expectedErr)
		})
		t.Run(tc.name+"/text", func(t *testing.T) {
			r := Renderer{Format: FormatText, Out: fakeWriter{err: tc.writeErr}}
			err := r.Render(Envelope{}, func(w io.Writer) error {
				_, werr := w.Write([]byte("hi\n"))
				return werr
			})
			if tc.expectedNoOp {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.expectedErr)
		})
	}
}

// TestRendererPropagatesMarshalError pins that envelope-marshal failures
// are NOT swallowed by the EPIPE filter — they're a programming error in
// the subcommand (handed Renderer un-marshalable Data) and must surface
// so the regression is caught.
func TestRendererPropagatesMarshalError(t *testing.T) {
	var buf bytes.Buffer
	r := Renderer{Format: FormatJSON, Out: &buf}
	// Invalid JSON in RawMessage causes MarshalIndent to fail.
	err := r.Render(Envelope{Data: json.RawMessage("not json")}, nil)
	require.ErrorContains(t, err, "marshal envelope")
	require.Empty(t, buf.String(), "no partial output should be written when marshalling fails")
}

// TestRendererRejectsUnsupportedFormat covers the defensive default
// branch that fires only if a caller bypasses ParseFormat (e.g.
// constructing a Renderer with Format="xml" directly).
func TestRendererRejectsUnsupportedFormat(t *testing.T) {
	var buf bytes.Buffer
	r := Renderer{Format: Format("xml"), Out: &buf}
	err := r.Render(Envelope{}, nil)
	require.ErrorContains(t, err, `unsupported output format "xml"`)
}

// TestRenderError covers the four key contract points of RenderError:
// JSON success returns ErrRendered with a well-formed error envelope,
// text mode passes the failure through unchanged, a write failure
// during rendering preserves the original failure via errors.Join, and
// pre-existing errors on the envelope are retained (append semantics).
func TestRenderError(t *testing.T) {
	origFailure := errors.New("parser module missing")

	t.Run("json mode returns ErrRendered with error envelope", func(t *testing.T) {
		var buf bytes.Buffer
		r := Renderer{Format: FormatJSON, Out: &buf}
		env := Envelope{
			ParserVersion:    "v0.26.2",
			ConnectionStatus: ConnectionDisconnected,
			Data:             json.RawMessage(`{"binary_version":"dev"}`),
		}
		err := r.RenderError(env, origFailure)
		require.ErrorIs(t, err, ErrRendered)

		var got Envelope
		require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
		require.Len(t, got.Errors, 1)
		require.Equal(t, "internal_error", got.Errors[0].Code)
		require.Equal(t, SeverityError, got.Errors[0].Severity)
		require.Equal(t, origFailure.Error(), got.Errors[0].Message)
		require.Empty(t, got.Data, "Data must be cleared on error path")
		require.Equal(t, "v0.26.2", got.ParserVersion,
			"envelope context fields should be preserved")
	})

	t.Run("text mode returns original failure unchanged", func(t *testing.T) {
		var buf bytes.Buffer
		r := Renderer{Format: FormatText, Out: &buf}
		err := r.RenderError(Envelope{}, origFailure)
		require.ErrorIs(t, err, origFailure)
		require.Empty(t, buf.String(), "text mode must not write anything")
	})

	t.Run("write failure preserves original via errors.Join", func(t *testing.T) {
		writeErr := errors.New("disk full")
		r := Renderer{Format: FormatJSON, Out: fakeWriter{err: writeErr}}
		err := r.RenderError(Envelope{}, origFailure)
		require.ErrorIs(t, err, origFailure, "original failure must be preserved")
		require.ErrorContains(t, err, "render error envelope")
		require.False(t, errors.Is(err, ErrRendered),
			"must not return ErrRendered when rendering itself failed")
	})

	t.Run("pre-existing errors preserved via append", func(t *testing.T) {
		var buf bytes.Buffer
		r := Renderer{Format: FormatJSON, Out: &buf}
		existing := Error{
			Code:     "42703",
			Severity: SeverityWarning,
			Message:  "column not found",
		}
		env := Envelope{
			ConnectionStatus: ConnectionDisconnected,
			Errors:           []Error{existing},
		}
		err := r.RenderError(env, origFailure)
		require.ErrorIs(t, err, ErrRendered)

		var got Envelope
		require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
		require.Len(t, got.Errors, 2, "must have both pre-existing and new error")
		require.Equal(t, "42703", got.Errors[0].Code)
		require.Equal(t, "internal_error", got.Errors[1].Code)
	})
}

// TestSeverityValues pins the wire string for every Severity constant.
// Agents key off these literals (they match the PostgreSQL fe/be
// protocol severity names); a typo here would silently change the
// envelope contract.
func TestSeverityValues(t *testing.T) {
	tests := []struct {
		name     string
		severity Severity
		expected string
	}{
		{name: "error", severity: SeverityError, expected: "ERROR"},
		{name: "warning", severity: SeverityWarning, expected: "WARNING"},
		{name: "notice", severity: SeverityNotice, expected: "NOTICE"},
		{name: "fatal", severity: SeverityFatal, expected: "FATAL"},
		{name: "panic", severity: SeverityPanic, expected: "PANIC"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, string(tc.severity))
		})
	}
}

// TestRendererJSON marshals the envelope and ignores the text closure.
// The trailing newline is part of the contract: terminals expect
// well-formed lines.
func TestRendererJSON(t *testing.T) {
	var buf bytes.Buffer
	r := Renderer{Format: FormatJSON, Out: &buf}
	err := r.Render(
		Envelope{ParserVersion: "v0.26.2", ConnectionStatus: ConnectionDisconnected},
		func(io.Writer) error {
			t.Fatal("text closure must not run in JSON mode")
			return nil
		},
	)
	require.NoError(t, err)
	require.True(t, bytes.HasSuffix(buf.Bytes(), []byte("\n")),
		"renderer must end JSON output with a newline; got %q", buf.String())

	var got Envelope
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Equal(t, "v0.26.2", got.ParserVersion)
	require.Equal(t, ConnectionDisconnected, got.ConnectionStatus)
}
