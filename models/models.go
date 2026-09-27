// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package models is the model boundary of spec 007: the Model a harness
// streams one request through, the Connection that names where the
// request goes, the catalog of model figures, and cost. The package
// dials nothing; models/dialect is the implementation over HTTP and
// models/scripted plays a script for tests.
package models

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"
)

// Families of model. The family picks the codec a thread encodes with.
const (
	FamilyAnthropic = "anthropic"
	FamilyOpenAI    = "openai"
	FamilyOther     = "other"
)

// DefaultDialect is the wire dialect a family speaks when the connection
// names none.
func DefaultDialect(family string) ir.Dialect {
	switch family {
	case FamilyAnthropic:
		return ir.DialectAnthropicMessages
	case FamilyOpenAI:
		return ir.DialectOpenAIResponses
	}
	return ir.DialectOpenAIChat
}

// SchemeScripted is the URL scheme of a connection to the scripted model
// of spec 026. Only tests and the topos client accept it.
const SchemeScripted = "scripted"

// Connection names where a request goes. The zero Connection is invalid:
// there is no implicit model.
type Connection struct {
	BaseURL string     `json:"base_url"`
	Model   string     `json:"model"`
	Family  string     `json:"family,omitempty"`
	Dialect ir.Dialect `json:"dialect,omitempty"`
	// Credential is resolved for each call and never recorded.
	Credential string `json:"-"`
}

// Validate refuses a connection with no base URL or no model, and one
// whose family or dialect is outside the vocabulary.
func (c Connection) Validate() error {
	if c.BaseURL == "" {
		return errors.New("models: the connection has no base URL")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return fmt.Errorf("models: the connection's base URL: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
		if u.Host == "" {
			return fmt.Errorf("models: the connection's base URL %q has no host", c.BaseURL)
		}
	case SchemeScripted:
	default:
		return fmt.Errorf("models: the connection's base URL %q is not http, https or scripted", c.BaseURL)
	}
	if strings.TrimSpace(c.Model) == "" {
		return errors.New("models: the connection names no model")
	}
	switch c.Family {
	case "", FamilyAnthropic, FamilyOpenAI, FamilyOther:
	default:
		return fmt.Errorf("models: family %q is not anthropic, openai or other", c.Family)
	}
	switch c.Dialect {
	case "", ir.DialectAnthropicMessages, ir.DialectOpenAIResponses, ir.DialectOpenAIChat:
	default:
		return fmt.Errorf("models: dialect %q is not anthropic-messages, openai-responses or openai-chat", c.Dialect)
	}
	return nil
}

// Scripted reports whether the connection reaches the scripted model.
func (c Connection) Scripted() bool {
	return strings.HasPrefix(c.BaseURL, SchemeScripted+":")
}

// EffectiveDialect is the connection's dialect, or its family's default.
func (c Connection) EffectiveDialect() ir.Dialect {
	if c.Dialect != "" {
		return c.Dialect
	}
	return DefaultDialect(c.Family)
}

// Model streams one request to a model.
type Model interface {
	Stream(ctx context.Context, req Request) (Stream, error)
}

// Request is one model request: the IR the harness built from the fold,
// the connection, and whether to keep the request bytes.
type Request struct {
	IR         ir.Request
	Connection Connection
	Capture    bool
}

// Stream is one response as it arrives. Next returns io.EOF after the
// terminal event, and Result is valid after that.
type Stream interface {
	Next() (ir.Event, error)
	Result() Result
	Close() error
}

// Result is what a finished stream leaves for the log.
type Result struct {
	// Message is the assistant message as the Lux wire message, every
	// block verbatim.
	Message    lux.Message
	StopReason ir.StopReason
	Usage      lux.Usage
	// RawResponse is the response body as received.
	RawResponse []byte
	// RequestBytes is the exact request body, kept only when the
	// request asked for capture.
	RequestBytes  []byte
	RequestSHA256 string
	RequestSize   int64
	// Codec is "<dialect>@<llmdialect module version>".
	Codec string
	// Loss is what the encoding dropped, from the request's loss report.
	Loss []string
	// FirstToken is the time from sending to the first content event.
	FirstToken time.Duration
}

// HTTPError is a model server's error answer.
type HTTPError struct {
	Status     int
	Type       string
	Message    string
	Body       []byte
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	if e.Type != "" || e.Message != "" {
		return fmt.Sprintf("models: HTTP %d: %s: %s", e.Status, e.Type, e.Message)
	}
	return fmt.Sprintf("models: HTTP %d", e.Status)
}

// StreamError is an error event inside a response stream.
type StreamError struct {
	Err error
}

func (e *StreamError) Error() string { return "models: the stream failed: " + e.Err.Error() }
func (e *StreamError) Unwrap() error { return e.Err }

// TransportError is a failure to reach the model or to read its answer:
// a refused or reset connection, a timeout, a body cut short.
type TransportError struct {
	Err error
}

func (e *TransportError) Error() string { return "models: transport: " + e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// EncodeError is the codec refusing to encode a request.
type EncodeError struct {
	Err error
}

func (e *EncodeError) Error() string { return "models: encode the request: " + e.Err.Error() }
func (e *EncodeError) Unwrap() error { return e.Err }

// ErrIncomplete is a stream that ended before its terminal event.
var ErrIncomplete = errors.New("models: the stream ended before its terminal event")

// retryableStatus are the HTTP answers spec 005 retries.
var retryableStatus = map[int]bool{408: true, 429: true, 500: true, 502: true, 503: true, 504: true, 529: true}

// retryableStream are the error types inside a stream spec 005 retries:
// an overloaded model, a rate limit, or a server fault.
var retryableStream = []string{"overloaded", "rate_limit", "server_error", "api_error", "internal_error"}

// spendRefusals are the error types a gateway answers when the budget it
// holds for the caller is spent. Lux sends them as HTTP 429, a status
// otherwise retried; a spent budget does not come back by waiting.
var spendRefusals = []string{"budget_exhausted", "spend_exceeded"}

// SpendError is a spend refusal from a core other than the model
// gateway: Cella refusing a sandbox create or a command because the
// session's allowance for sandbox time is spent. Code is one of the
// spend refusals; the gateway's own arrive as HTTPError, and
// SpendRefused reads both.
type SpendError struct {
	// Core names the core that refused, such as "cella".
	Core string
	Code string
	Err  error
}

func (e *SpendError) Error() string {
	return "models: " + e.Core + " refused for spend (" + e.Code + "): " + e.Err.Error()
}

func (e *SpendError) Unwrap() error { return e.Err }

// SpendCode reports whether a core's refusal code is a spend refusal.
func SpendCode(code string) bool { return slices.Contains(spendRefusals, code) }

// SpendRefused reports whether err is a core refusing a request because
// the caller's budget is spent, the model gateway or another core, and
// names the refusal.
func SpendRefused(err error) (string, bool) {
	var he *HTTPError
	if errors.As(err, &he) && SpendCode(he.Type) {
		return he.Type, true
	}
	var se *SpendError
	if errors.As(err, &se) && SpendCode(se.Code) {
		return se.Code, true
	}
	return "", false
}

// Retryable reports whether spec 005 retries err.
func Retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if _, spent := SpendRefused(err); spent {
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return retryableStatus[he.Status]
	}
	var se *StreamError
	if errors.As(err, &se) {
		text := strings.ToLower(se.Err.Error())
		for _, t := range retryableStream {
			if strings.Contains(text, t) {
				return true
			}
		}
		return false
	}
	var te *TransportError
	return errors.As(err, &te) || errors.Is(err, ErrIncomplete)
}

// RetryAfter is the delay a model server asked for, or zero.
func RetryAfter(err error) time.Duration {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.RetryAfter
	}
	return 0
}

// The error codes of spec 007.
const (
	CodeCredentialMissing = "model_credential_missing"
	CodeUnknown           = "model_unknown"
	CodeUnpriced          = "model_unpriced"
	CodeMismatch          = "codec_mismatch"
)

// Coded is an error with one of the codes above.
type Coded struct {
	Code    string
	Message string
}

func (e *Coded) Error() string { return "models: " + e.Code + ": " + e.Message }
