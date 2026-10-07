// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package server is toposd's public API (spec 015): one route table
// that the router, the action test and the OpenAPI document all read,
// every request verified and asked of the authorizer (spec 006), one
// error envelope, paging, idempotency and the session event stream.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/ratelimit"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/internal/triggers"
	"latere.ai/x/topos/machine"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
)

// Bounds of spec 015.
const (
	DefaultBasePath  = "/v1"
	MaxBody          = 1 << 20
	MaxEventBody     = 8 << 20
	MaxLimit         = 500
	RequestsPerMin   = 600
	IdempotencyTTL   = 24 * time.Hour
	DefaultHeartbeat = 15 * time.Second
	// StreamsPerSubject is how many event streams one subject may hold
	// open at once on one server.
	StreamsPerSubject = 16
)

// Authenticator verifies a request's bearer; auth.Verifier is the one
// toposd runs.
type Authenticator interface {
	Authenticate(r *http.Request) (auth.Caller, error)
}

// Options configure a Server.
type Options struct {
	Sessions session.Store
	Objects  store.Store
	Verifier Authenticator
	Guard    auth.Guard
	// PublicURL is TOPOS_PUBLIC_URL, the base of every URL an answer
	// carries.
	PublicURL string
	// BasePath is TOPOS_BASE_PATH, the path the API answers under in the
	// place of DefaultBasePath (spec 030). Set, it is the path of
	// PublicURL, which is then the API root itself; empty, the API
	// answers under DefaultBasePath and its root is PublicURL with
	// DefaultBasePath after it.
	BasePath string
	// Heartbeat is the stream's comment interval; DefaultHeartbeat when
	// zero.
	Heartbeat time.Duration
	// PerMinute is the rate limit per subject; RequestsPerMin when zero,
	// none when negative.
	PerMinute int
	// MaxStreams bounds the event streams one subject holds open at once;
	// StreamsPerSubject when zero, none when negative.
	MaxStreams int
	Now        func() time.Time
	Log        *slog.Logger
	// Notify is called once a hosted session has new input, a first
	// message or a sent event, so the server's runners claim it without
	// waiting for their next poll. Nil notifies nobody.
	Notify func()
	// HostSessions is TOPOS_HOST_SESSIONS=on: a session whose agent asks
	// for a host machine runs on the server's own host. Off, it is
	// refused machine_unavailable.
	HostSessions bool
	// Cella is TOPOS_CELLA_URL set: the server's runners create Cella
	// sandboxes, so a session of an agent that names no machine runs on
	// one when host sessions are off.
	Cella bool
	// Sink receives, as spec 023's sink envelope carries them, a mutation
	// the authorizer allowed that the server then refused: a fork whose
	// message's session.send was refused after its session.fork was
	// allowed (spec 056), which an authorizer that recorded the fork at
	// that allow reads to close the record; and a change of a session's
	// metadata, which no event records (spec 057), with the keys it
	// changed and none of their values. Its error is logged and changes no
	// answer. Nil logs the event.
	Sink func(ctx context.Context, e SinkEvent) error
	// Deleted is called after a session is deleted, to remove what the
	// server keeps for it outside the store, such as a host session's
	// directories. Nil removes nothing.
	Deleted func(id string) error
	// Identities is the identity provider that hosts the installation's
	// agents (spec 018); nil gives agents no identity.
	Identities Identities
	// Runnable answers whether the installation runs a session's model,
	// by the rule its runner connects it with (spec 007), at the create
	// of a session, at a switch of its model (spec 015), and at a send
	// whose allow moves the session to another (spec 038): nil, a
	// models.Coded model_unknown for a model no source gives a window and
	// an output limit, or another error for a model whose figures could
	// not be read. hosted.Runnable is the one toposd runs; nil answers
	// from the embedded catalog alone.
	Runnable func(ctx context.Context, m v1.AgentModel, overlay models.Entry) error
	// Workspaces reads a session's working directory as it is now, for
	// GET /sessions/{id}/files (spec 044), and never starts or creates a
	// machine: one that does not run is machine.ErrNotRunning.
	// hosted.Workspaces is the one toposd runs; nil answers every read
	// file_unavailable.
	Workspaces func(ctx context.Context, s session.Session) (machine.FileReader, error)
}

// Server answers the API.
type Server struct {
	o       Options
	limits  *ratelimit.Buckets
	streams *slots
	routes  []route
	// triggers fires the installation's triggers (spec 022).
	triggers *triggers.Engine
	// root is the path every route is served under, and rootURL the
	// absolute URL of that path, the base of every URL an answer writes.
	root, rootURL string
}

// New builds the server over its stores.
func New(o Options) (*Server, error) {
	switch {
	case o.Sessions == nil || o.Objects == nil:
		return nil, errors.New("server: a store is missing")
	case o.Verifier == nil || o.Guard.Authorizer == nil:
		return nil, errors.New("server: the verifier or the authorizer is missing")
	case o.PublicURL == "":
		return nil, errors.New("server: the public URL is empty")
	}
	o.PublicURL = strings.TrimRight(o.PublicURL, "/")
	root, rootURL, err := apiRoot(o.PublicURL, o.BasePath)
	if err != nil {
		return nil, err
	}
	if o.Heartbeat <= 0 {
		o.Heartbeat = DefaultHeartbeat
	}
	if o.PerMinute == 0 {
		o.PerMinute = RequestsPerMin
	}
	if o.MaxStreams == 0 {
		o.MaxStreams = StreamsPerSubject
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Notify == nil {
		o.Notify = func() {}
	}
	if o.Runnable == nil {
		cat, err := models.Embedded()
		if err != nil {
			return nil, err
		}
		o.Runnable = func(_ context.Context, m v1.AgentModel, overlay models.Entry) error {
			_, err := cat.Resolve(m.Name, overlay)
			return err
		}
	}
	s := &Server{o: o, limits: ratelimit.New(ratelimit.Config{PerMinute: o.PerMinute, Now: o.Now}), streams: newSlots(o.MaxStreams), root: root, rootURL: rootURL}
	s.routes = table()
	if s.triggers, err = triggers.New(triggers.Options{Store: o.Objects, Sessions: o.Sessions, Actor: actor{s}, Now: o.Now, Log: o.Log}); err != nil {
		return nil, err
	}
	for _, rt := range s.routes {
		if len(rt.actions) == 0 && !rt.public {
			return nil, fmt.Errorf("server: %s %s asks no action", rt.method, rt.path)
		}
	}
	return s, nil
}

// apiRoot is the path the routes are served under and its absolute URL:
// DefaultBasePath under publicURL when basePath is empty, and otherwise
// basePath, which must then be publicURL's own path, so that publicURL
// is the root and no URL an answer writes carries the base path twice.
func apiRoot(publicURL, basePath string) (root, rootURL string, err error) {
	if basePath == "" {
		return DefaultBasePath, publicURL + DefaultBasePath, nil
	}
	u, err := url.Parse(publicURL)
	if err != nil || u.Path != basePath {
		return "", "", fmt.Errorf("server: the base path %s is not the path of the public URL %s", basePath, publicURL)
	}
	return basePath, publicURL, nil
}

// Handler is the router: every route of the table under the base path.
// Every other path, outside the base path as well as under it, answers
// not_found, so the router mounts at the listener's root beneath the
// probes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for i := range s.routes {
		rt := &s.routes[i]
		mux.HandleFunc(rt.method+" "+s.root+rt.path, func(w http.ResponseWriter, r *http.Request) { s.serve(w, r, rt) })
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, s.o.Log, refuse(CodeNotFound, "no route"))
	})
	return mux
}

// route is one row of the table.
type route struct {
	method, path string
	// actions are the questions the route may ask, its own first; a
	// public route asks none.
	actions []string
	public  bool
	op      string
	// summary names the route's action in at most maxSummaryWords
	// words, the label a reference lists the operation by; what the
	// route does is its description, in opDescriptions.
	summary string
	// status is the answer's status on success.
	status int
	// creates marks a route that answers 201 Created in the place of
	// status when the request creates its object, as an apply does.
	creates bool
	// body bounds the request body; zero reads none.
	body   int64
	handle func(c *call) error
}

// call is one request under its route.
type call struct {
	s      *Server
	w      http.ResponseWriter
	r      *http.Request
	rt     *route
	caller auth.Caller
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, rt *route) {
	if r.Header.Get("X-Request-Id") == "" {
		r.Header.Set("X-Request-Id", "req_"+rand.Text())
	}
	w.Header().Set("X-Request-Id", r.Header.Get("X-Request-Id"))
	c := &call{s: s, w: w, r: r, rt: rt}
	if !rt.public {
		caller, err := s.o.Verifier.Authenticate(r)
		if err != nil {
			writeError(w, s.o.Log, err)
			return
		}
		c.caller = caller
		if a := s.limits.Allow(caller.Subject); !a.OK {
			e := refuse(CodeRateLimited, "%d requests a minute", a.PerMinute)
			e.retryAfter = a.Retry
			writeError(w, s.o.Log, e)
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, max(rt.body, 1))
	if r.Method == http.MethodPost && r.Header.Get("Idempotency-Key") != "" {
		s.idempotent(r.Context(), c)
		return
	}
	if err := rt.handle(c); err != nil {
		writeError(w, s.o.Log, err)
	}
}

// ask asks the route's question, or one of the others it names.
func (c *call) ask(ctx context.Context, action string, res authz.Resource) (authz.Decision, error) {
	if !slices.Contains(c.rt.actions, action) {
		return authz.Decision{}, fmt.Errorf("server: %s %s asked %s, which its row does not name", c.rt.method, c.rt.path, action)
	}
	return c.s.o.Guard.Ask(ctx, auth.Envelope(c.caller, action, res, c.r))
}

// askLimits asks a question whose allow may carry limits, and decodes
// them.
func (c *call) askLimits(ctx context.Context, action string, res authz.Resource) (authorizer.Limits, error) {
	if !slices.Contains(c.rt.actions, action) {
		return authorizer.Limits{}, fmt.Errorf("server: %s %s asked %s, which its row does not name", c.rt.method, c.rt.path, action)
	}
	return c.s.o.Guard.Limits(ctx, auth.Envelope(c.caller, action, res, c.r))
}

// body reads the request body.
func (c *call) body() ([]byte, error) {
	b, err := io.ReadAll(c.r.Body)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// decode reads a JSON body into v, refusing unknown fields.
func (c *call) decode(v any) error {
	b, err := c.body()
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return refuse(CodeInvalidRequest, "the body is empty")
	}
	return decodeBody(b, v)
}

// decodeOptional is decode for a route whose body may be left out, which
// leaves v as it is.
func (c *call) decodeOptional(v any) error {
	b, err := c.body()
	if err != nil || len(strings.TrimSpace(string(b))) == 0 {
		return err
	}
	return decodeBody(b, v)
}

// decodeBody reads one JSON value into v, refusing unknown fields.
func decodeBody(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return refuse(CodeInvalidRequest, "the body does not decode: %v", err)
	}
	if dec.More() {
		return refuse(CodeInvalidRequest, "the body holds more than one JSON value")
	}
	return nil
}

// reply writes v as JSON, as the API answers it, with the route's
// success status, or status when it is set.
func (c *call) reply(status int, v any) error {
	v, err := asAnswer(v)
	if err != nil {
		return err
	}
	b, err := session.Marshal(v)
	if err != nil {
		return err
	}
	if status == 0 {
		status = c.rt.status
	}
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(status)
	_, err = c.w.Write(append(b, '\n'))
	return err
}

// url is the absolute URL of a path under the API root, as a client
// outside reaches it.
func (c *call) url(path string, q url.Values) string {
	u := c.s.rootURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// pageParams reads limit and cursor.
func (c *call) pageParams() (int, string, error) {
	q := c.r.URL.Query()
	limit := session.DefaultListLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxLimit {
			return 0, "", refuse(CodeInvalidRequest, "limit is %q, not between 1 and %d", v, MaxLimit)
		}
		limit = n
	}
	return limit, q.Get("cursor"), nil
}

// page is a list answer.
type page struct {
	Items      any    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// replyPage writes one page, with the next page's URL in a Link header.
func (c *call) replyPage(items any, next string) error {
	if next != "" {
		q := c.r.URL.Query()
		q.Set("cursor", next)
		c.w.Header().Add("Link", "<"+c.url(strings.TrimPrefix(c.r.URL.Path, c.s.root), q)+`>; rel="next"`)
	}
	return c.reply(http.StatusOK, page{Items: items, NextCursor: next})
}

// idempotent answers a POST that carries an Idempotency-Key: a repeat
// from the same subject with the same body within the window answers
// the stored answer, and the same key with another body is refused.
func (s *Server) idempotent(ctx context.Context, c *call) {
	body, err := c.body()
	if err != nil {
		writeError(c.w, s.o.Log, err)
		return
	}
	sum := sha256.Sum256(body)
	rec := store.Idempotency{
		Subject: c.caller.Subject, Key: c.r.Header.Get("Idempotency-Key"),
		Route: c.rt.method + " " + c.r.URL.Path, BodyHash: hex.EncodeToString(sum[:]),
		ExpiresAt: s.o.Now().Add(IdempotencyTTL),
	}
	// The record outlives a caller that hangs up mid-answer.
	ctx = context.WithoutCancel(ctx)
	held, fresh, err := s.o.Objects.Begin(ctx, rec)
	switch {
	case err != nil:
		writeError(c.w, s.o.Log, err)
		return
	case !fresh && (held.Route != rec.Route || held.BodyHash != rec.BodyHash):
		writeError(c.w, s.o.Log, refuse(CodeIdempotencyConflict, "the key was first used on %s with another body", held.Route))
		return
	case !fresh && !held.Done:
		writeError(c.w, s.o.Log, refuse(CodeConflict, "the first request with this key is still being answered"))
		return
	case !fresh:
		c.w.Header().Set("Content-Type", held.ContentType)
		c.w.Header().Set("Idempotent-Replayed", "true")
		c.w.WriteHeader(held.Status)
		if _, err := c.w.Write(held.Body); err != nil {
			s.o.Log.WarnContext(ctx, "replay an idempotent answer", "err", err)
		}
		return
	}
	c.r.Body = io.NopCloser(bytes.NewReader(body))
	rw := &recorder{ResponseWriter: c.w, status: http.StatusOK}
	c.w = rw
	if err := c.rt.handle(c); err != nil {
		writeError(rw, s.o.Log, err)
	}
	if rw.status >= http.StatusInternalServerError {
		if err := s.o.Objects.Abandon(ctx, rec.Subject, rec.Key); err != nil {
			s.o.Log.ErrorContext(ctx, "abandon an idempotency key", "err", err)
		}
		return
	}
	rec.Status, rec.ContentType, rec.Body = rw.status, rw.Header().Get("Content-Type"), rw.body.Bytes()
	if err := s.o.Objects.Finish(ctx, rec); err != nil {
		s.o.Log.ErrorContext(ctx, "store an idempotent answer", "err", err)
	}
}

// recorder keeps a copy of the answer it passes on, for the idempotency
// record.
type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}
