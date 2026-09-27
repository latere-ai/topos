// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package dialect is the models.Model over HTTP (spec 007): it encodes
// the IR with the llmdialect backend codec of the connection's dialect,
// posts the bytes, decodes the stream with the same codec, and hands the
// harness the IR events and the assistant message as the Lux wire
// message the log stores. Topos writes no provider adapter.
package dialect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"latere.ai/x/pkg/llmdialect"
	"latere.ai/x/pkg/llmdialect/anthropic"
	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/openaichat"
	"latere.ai/x/pkg/llmdialect/openairesp"
	"latere.ai/x/pkg/otel"

	"latere.ai/x/topos/models"
)

// AnthropicVersion is the Messages API version the requests name.
const AnthropicVersion = "2023-06-01"

// maxErrorBody bounds how much of an error answer is read.
const maxErrorBody = 1 << 20

// Model is the HTTP model. The zero Model sends through the family's
// instrumented client, so every request carries its trace.
type Model struct {
	Client *http.Client
	// Now is the clock for the first-token time; nil is time.Now.
	Now func() time.Time

	once   sync.Once
	client *http.Client
}

func (m *Model) httpClient() *http.Client {
	m.once.Do(func() {
		m.client = m.Client
		if m.client == nil {
			m.client = otel.HTTPClient()
		}
	})
	return m.client
}

// codec returns the backend codec of a dialect and its request path.
func codec(d ir.Dialect) (llmdialect.Backend, string, error) {
	switch d {
	case ir.DialectAnthropicMessages:
		return anthropic.NewBackend(anthropic.BackendOptions{}), "/v1/messages", nil
	case ir.DialectOpenAIResponses:
		return openairesp.NewBackend(), "/v1/responses", nil
	case ir.DialectOpenAIChat:
		return openaichat.NewBackend(openaichat.BackendOptions{}), "/v1/chat/completions", nil
	}
	return nil, "", fmt.Errorf("models: no codec for dialect %q", d)
}

// CodecVersion is "<dialect>@<llmdialect module version>" of this build.
func CodecVersion(d ir.Dialect) string {
	v := "(devel)"
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range bi.Deps {
			if dep.Path == "latere.ai/x/pkg" {
				v = dep.Version
				if dep.Replace != nil {
					v = dep.Replace.Version
				}
			}
		}
	}
	return string(d) + "@" + v
}

// encoded is a request in its connection's dialect: the codec, the body,
// the path it is posted to, and what the encoding lost.
type encoded struct {
	be   llmdialect.Backend
	body []byte
	path string
	loss []string
}

func encode(req models.Request) (encoded, error) {
	be, path, err := codec(req.Connection.EffectiveDialect())
	if err != nil {
		return encoded{}, err
	}
	ireq := req.IR
	ireq.Model = req.Connection.Model
	ireq.Stream = true
	body, err := be.EncodeRequest(&ireq)
	if err != nil {
		return encoded{}, &models.EncodeError{Err: err}
	}
	return encoded{be: be, body: body, path: path, loss: ireq.Loss.Strings()}, nil
}

// Encode is the body Stream would send for req: the bytes a replay
// hashes against a recorded request (spec 007).
func (m *Model) Encode(req models.Request) ([]byte, error) {
	enc, err := encode(req)
	return enc.body, err
}

// Codec is CodecVersion, the codec this build encodes a dialect with.
func (m *Model) Codec(d ir.Dialect) string { return CodecVersion(d) }

// Stream sends one request and returns its stream.
func (m *Model) Stream(ctx context.Context, req models.Request) (models.Stream, error) {
	conn := req.Connection
	if err := conn.Validate(); err != nil {
		return nil, err
	}
	if conn.Scripted() {
		return nil, errors.New("models: a scripted connection is played by models/scripted, not sent over HTTP")
	}
	d := conn.EffectiveDialect()
	enc, err := encode(req)
	if err != nil {
		return nil, err
	}
	body := enc.body
	sum := sha256.Sum256(body)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(conn.BaseURL, "/")+enc.path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("models: build the request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	if conn.Credential != "" {
		if d == ir.DialectAnthropicMessages {
			hreq.Header.Set("x-api-key", conn.Credential)
		} else {
			hreq.Header.Set("Authorization", "Bearer "+conn.Credential)
		}
	}
	if d == ir.DialectAnthropicMessages {
		hreq.Header.Set("anthropic-version", AnthropicVersion)
	}
	now := m.Now
	if now == nil {
		now = time.Now
	}
	sent := now()
	resp, err := m.httpClient().Do(hreq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &models.TransportError{Err: err}
	}
	if resp.StatusCode/100 != 2 {
		return nil, httpError(resp, now())
	}
	s := &stream{
		body:    resp.Body,
		dialect: d,
		now:     now,
		sent:    sent,
		result: models.Result{
			RequestSHA256: hex.EncodeToString(sum[:]),
			RequestSize:   int64(len(body)),
			Codec:         CodecVersion(d),
			Loss:          enc.loss,
		},
	}
	if req.Capture {
		s.result.RequestBytes = body
	}
	s.reader = &recordingReader{r: resp.Body, raw: &s.raw}
	s.dec = enc.be.NewEventDecoder(s.reader)
	return s, nil
}

// httpError reads a model server's error answer.
func httpError(resp *http.Response, now time.Time) error {
	b, rerr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	cerr := resp.Body.Close()
	e := &models.HTTPError{Status: resp.StatusCode, Body: b, RetryAfter: retryAfter(resp.Header.Get("Retry-After"), now)}
	var body struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Error   *struct {
			Type    string `json:"type"`
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &body) == nil {
		e.Type, e.Message = body.Type, body.Message
		if body.Error != nil {
			e.Type, e.Message = body.Error.Type, body.Error.Message
			if e.Type == "" && body.Error.Code != nil {
				e.Type = fmt.Sprint(body.Error.Code)
			}
		}
	}
	return errors.Join(e, rerr, cerr)
}

// retryAfter reads a Retry-After header: seconds, or an HTTP date.
func retryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

// recordingReader keeps every byte read, and the first read error that
// is not the end of the body, so a failed stream can be told apart as a
// transport failure or an error the stream itself carried.
type recordingReader struct {
	r   io.Reader
	raw *bytes.Buffer
	err error
}

func (r *recordingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.raw.Write(p[:n])
	if err != nil && !errors.Is(err, io.EOF) && r.err == nil {
		r.err = err
	}
	return n, err
}

type stream struct {
	body    io.ReadCloser
	reader  *recordingReader
	dec     ir.EventDecoder
	dialect ir.Dialect
	now     func() time.Time
	sent    time.Time
	raw     bytes.Buffer

	acc    models.Accumulator
	result models.Result
	done   bool
	failed error
}

func (s *stream) Next() (ir.Event, error) {
	if s.failed != nil {
		return ir.Event{}, s.failed
	}
	if s.done {
		return ir.Event{}, io.EOF
	}
	ev, err := s.dec.Next()
	if errors.Is(err, io.EOF) {
		if !s.acc.Stopped() || !terminated(s.dialect, s.raw.Bytes()) {
			s.failed = models.ErrIncomplete
			if s.reader.err != nil {
				s.failed = &models.TransportError{Err: s.reader.err}
			}
			return ir.Event{}, s.failed
		}
		if err := s.finish(); err != nil {
			s.failed = err
			return ir.Event{}, err
		}
		s.done = true
		return ir.Event{}, io.EOF
	}
	if err != nil {
		switch {
		case s.reader.err != nil:
			s.failed = &models.TransportError{Err: s.reader.err}
		case errors.Is(err, io.ErrUnexpectedEOF):
			s.failed = models.ErrIncomplete
		default:
			s.failed = &models.StreamError{Err: err}
		}
		return ir.Event{}, s.failed
	}
	if s.result.FirstToken == 0 && content(ev.Type) {
		s.result.FirstToken = s.now().Sub(s.sent)
	}
	if err := s.acc.Add(ev); err != nil {
		s.failed = &models.StreamError{Err: err}
		return ir.Event{}, s.failed
	}
	return ev, nil
}

// terminal are the frames that end a stream, per dialect. The decoders
// write a well-formed tail when the upstream closes early, so a stream
// is complete only when its own terminal frame arrived.
var terminal = map[ir.Dialect][][]byte{
	ir.DialectAnthropicMessages: {[]byte(`"type":"message_stop"`), []byte("event: message_stop")},
	ir.DialectOpenAIResponses:   {[]byte(`"type":"response.completed"`), []byte(`"type":"response.incomplete"`)},
	ir.DialectOpenAIChat:        {[]byte("data: [DONE]"), []byte(`"finish_reason":"`)},
}

func terminated(d ir.Dialect, raw []byte) bool {
	for _, t := range terminal[d] {
		if bytes.Contains(raw, t) {
			return true
		}
	}
	return false
}

func content(t ir.EventType) bool {
	switch t {
	case ir.EventBlockStart, ir.EventTextDelta, ir.EventThinkingDelta, ir.EventArgsDelta:
		return true
	}
	return false
}

func (s *stream) finish() error {
	msg, usage, err := models.LuxMessage(s.acc.Response())
	if err != nil {
		return err
	}
	s.result.Message = msg
	s.result.Usage = usage
	s.result.StopReason = s.acc.Response().StopReason
	s.result.RawResponse = bytes.Clone(s.raw.Bytes())
	return nil
}

func (s *stream) Result() models.Result { return s.result }

func (s *stream) Close() error { return s.body.Close() }

var (
	_ models.Model   = (*Model)(nil)
	_ models.Encoder = (*Model)(nil)
)
