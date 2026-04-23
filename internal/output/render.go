// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package output

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"syscall"
)

// Format selects how a subcommand serializes its result.
//
//	text — human-readable, subcommand-defined layout (default).
//	json — Envelope marshalled as indented JSON.
type Format string

// Format values.
const (
	FormatText Format = "text"
	FormatJSON Format = "json"
)

// ParseFormat validates a user-supplied --output value. Only "text" and
// "json" are accepted; any other value (including empty) produces an
// error that names the valid choices, so cobra's surfaced message is
// actionable.
func ParseFormat(s string) (Format, error) {
	switch Format(s) {
	case FormatText:
		return FormatText, nil
	case FormatJSON:
		return FormatJSON, nil
	default:
		return "", fmt.Errorf("invalid --output %q: valid choices are %q, %q", s, FormatText, FormatJSON)
	}
}

// Renderer writes a subcommand's result in the selected format.
//
// For FormatText, textFn owns the output: it receives the configured
// writer and produces whatever lines the subcommand wants (the envelope
// is ignored). For FormatJSON, the envelope is marshalled as indented
// JSON so piped output stays human-readable; agents that want compact
// form can pipe through `jq -c`. A trailing newline is appended so the
// output is a well-formed line on terminals.
//
// EPIPE handling: write failures with errno EPIPE are suppressed (return
// nil) because they are the normal outcome when a downstream consumer
// closes stdout early (e.g. piping into `head -n1`). Surfacing them as a
// non-zero exit with a "broken pipe" message is hostile for a tool meant
// to be piped, and partial output is acceptable. Marshal errors and the
// unsupported-format error are NOT subject to EPIPE filtering — they are
// propagated unchanged. On Windows the native broken-pipe error has a
// different value (ERROR_BROKEN_PIPE), so this check would not fire
// there; Windows is not a supported target, so we don't add a separate
// branch.
type Renderer struct {
	Format Format
	Out    io.Writer
}

// Render dispatches on r.Format. textFn must be non-nil for FormatText;
// it is ignored for FormatJSON.
//
// textFn contract (FormatText only): Render passes textFn's returned
// error through suppressEPIPE, so any error that wraps syscall.EPIPE
// will be silently dropped — even if it did not originate from a write
// to r.Out. To avoid accidental suppression, textFn should perform only
// writes (fmt.Fprintf, w.Write, etc.) and return their errors directly.
// Pre-write work (formatting, lookups) should happen before calling
// Render so that textFn's error space is limited to write failures.
func (r Renderer) Render(env Envelope, textFn func(io.Writer) error) error {
	switch r.Format {
	case FormatJSON:
		buf, err := json.MarshalIndent(env, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal envelope: %w", err)
		}
		_, err = r.Out.Write(append(buf, '\n'))
		return suppressEPIPE(err)
	case FormatText:
		return suppressEPIPE(textFn(r.Out))
	default:
		// Defensive: ParseFormat should have rejected this earlier.
		return fmt.Errorf("unsupported output format %q", r.Format)
	}
}

// RenderError is the JSON-mode error path. It appends a single Error
// describing failure to env, renders the envelope, and returns
// ErrRendered so the caller (and cmd/crdb-sql/main.go) know the
// failure has already been surfaced as structured output and should
// not be reprinted to stderr. In text mode it returns failure
// unchanged so cobra's existing "Error: ..." path runs.
//
// The envelope's Data field is cleared because the partial result that
// motivated env (parser version, connection status) is still meaningful
// context, but any subcommand payload would be misleading on the error
// path. ParserVersion and ConnectionStatus may be empty if the failure
// happened before they were resolved; that is acceptable, since the
// errors entry is the load-bearing field for agents in this case.
//
// If marshalling the error envelope itself fails, both the original
// failure and the marshal/write error are joined (via errors.Join) so
// cmd/crdb-sql/main.go's stderr fallback shows both causes. The joined
// error is NOT wrapped in ErrRendered so cmd/crdb-sql/main.go prints it.
//
// EPIPE edge case: if the downstream consumer has already closed the
// pipe, Render succeeds (EPIPE is suppressed) and RenderError returns
// ErrRendered even though nothing reached stdout. This is intentional:
// the consumer abandoned the pipe, so no output channel remains and
// the best we can do is exit non-zero.
func (r Renderer) RenderError(env Envelope, failure error) error {
	return r.RenderErrorEntry(env, failure, Error{
		Code:     "internal_error",
		Severity: SeverityError,
		Message:  failure.Error(),
	})
}

// RenderErrorEntry is the structured-error variant of RenderError for
// callers that have already constructed an Error themselves. The
// supplied entry is appended verbatim to env.Errors instead of the
// generic internal_error shape RenderError synthesizes; this lets
// callers preserve enriched fields (e.g. SQLSTATE Code, Category,
// Position) that the generic synthesis would not populate.
//
// failure is still required so the text-mode return path stays
// consistent: text mode bypasses the envelope and returns failure
// unchanged. entry.Message and failure.Error() are not required to
// match — JSON consumers see entry, text consumers see failure. All
// other contract points (ErrRendered sentinel, Data clearing, append
// semantics on pre-existing errors, errors.Join on render failure)
// match RenderError.
func (r Renderer) RenderErrorEntry(env Envelope, failure error, entry Error) error {
	if r.Format != FormatJSON {
		return failure
	}
	env.Errors = append(env.Errors, entry)
	env.Data = nil
	if err := r.Render(env, nil); err != nil {
		return errors.Join(failure, fmt.Errorf("render error envelope: %w", err))
	}
	return ErrRendered
}

// suppressEPIPE returns nil if err is a syscall.EPIPE (broken pipe from
// a downstream consumer closing stdout early); otherwise it returns err
// unchanged. See the Renderer doc for rationale.
func suppressEPIPE(err error) error {
	if err == nil || errors.Is(err, syscall.EPIPE) {
		return nil
	}
	return err
}
