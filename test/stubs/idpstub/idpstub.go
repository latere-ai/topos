// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package idpstub is a stub identity provider that hosts agent
// identities for one installation, as spec 018 asks of one: an HTTP
// server on loopback that serves the host's client_credentials token,
// the create, archive, disable and list of the identities it hosts, and
// the hosted mint of a token for one of them, one audience and one
// session. It answers the refusals of that contract with its codes, in
// the flat envelope {"error": code, "message": sentence}, records every
// request and every token it minted, and injects failures. Tokens are
// opaque strings the stub remembers, so a test reads what a token
// carries from the stub rather than by verifying a signature. It is a
// test artifact.
package idpstub

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The operations a Failure is injected on.
const (
	OpToken   = "token"
	OpPut     = "put"
	OpArchive = "archive"
	OpDisable = "disable"
	OpList    = "list"
	OpMint    = "mint"
)

// The identity states the list reports.
const (
	StatusActive   = "active"
	StatusArchived = "archived"
	StatusDisabled = "disabled"
)

// MaxLifetime is the longest a minted token lives, which is also the
// lifetime of a mint that names none.
const MaxLifetime = 15 * time.Minute

// Failure is a refusal answered in place of an operation.
type Failure struct {
	Status int
	Code   string
	// Times is how many requests are refused; zero is once.
	Times int
}

// Recorded is one request the stub received.
type Recorded struct {
	Op     string
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// Owner is the person or organization an identity belongs to.
type Owner struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Agent is one hosted identity.
type Agent struct {
	Subject   string
	Ref       string
	Name      string
	Owner     Owner
	AppliedBy string
	Status    string
	CreatedAt time.Time
}

// Minted is what one minted token carries.
type Minted struct {
	Token     string
	Subject   string
	Ref       string
	Audience  string
	SessionID string
	Workload  string
	ExpiresAt time.Time
}

// Server is the stub.
type Server struct {
	srv          *httptest.Server
	clientID     string
	clientSecret string

	mu         sync.Mutex
	audiences  []string
	hostTTL    time.Duration
	hostTokens map[string]time.Time
	agents     []*Agent
	minted     map[string]Minted
	failures   map[string][]Failure
	requests   []Recorded
	now        func() time.Time
}

// New starts a stub whose one host client is clientID with secret, and
// closes it when the test ends. It mints for the audiences cella, origo
// and arca until SetAudiences names others.
func New(t testing.TB, clientID, secret string) *Server {
	s := &Server{
		clientID: clientID, clientSecret: secret,
		audiences:  []string{"cella", "origo", "arca"},
		hostTTL:    time.Hour,
		hostTokens: map[string]time.Time{},
		minted:     map[string]Minted{},
		failures:   map[string][]Failure{},
		now:        time.Now,
	}
	s.srv = httptest.NewServer(s.routes())
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the identity provider's base URL.
func (s *Server) URL() string { return s.srv.URL }

// SetAudiences sets the audiences the host may mint for.
func (s *Server) SetAudiences(audiences ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audiences = audiences
}

// SetHostTokenLifetime sets how long a host token lives.
func (s *Server) SetHostTokenLifetime(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hostTTL = d
}

// Fail queues a refusal for the next requests of op.
func (s *Server) Fail(op string, f Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[op] = append(s.failures[op], f)
}

// Agents returns every identity the stub holds, oldest first.
func (s *Server) Agents() []Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Agent, len(s.agents))
	for i, a := range s.agents {
		out[i] = *a
	}
	return out
}

// HostTokens returns every host token the stub issued.
func (s *Server) HostTokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.hostTokens))
	for t := range s.hostTokens {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

// Token is what a token the stub minted carries.
func (s *Server) Token(token string) (Minted, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.minted[token]
	return m, ok
}

// Mints returns every token the stub minted.
func (s *Server) Mints() []Minted {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Minted, 0, len(s.minted))
	for _, m := range s.minted {
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b Minted) int { return strings.Compare(a.Token, b.Token) })
	return out
}

// Requests returns every request the stub received, in order.
func (s *Server) Requests() []Recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// Count is how many requests of op the stub received.
func (s *Server) Count(op string) int {
	n := 0
	for _, r := range s.Requests() {
		if r.Op == op {
			n++
		}
	}
	return n
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", s.admit(OpToken, false, s.token))
	mux.HandleFunc("PUT /hosted-agents/{ref}", s.admit(OpPut, true, s.put))
	mux.HandleFunc("POST /hosted-agents/{ref}/archive", s.admit(OpArchive, true, s.archive))
	mux.HandleFunc("POST /hosted-agents/{ref}/disable", s.admit(OpDisable, true, s.disable))
	mux.HandleFunc("GET /hosted-agents", s.admit(OpList, true, s.list))
	mux.HandleFunc("POST /actor-tokens", s.admit(OpMint, true, s.mint))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		refuse(w, http.StatusNotFound, "not_found", "the stub serves no "+r.Method+" "+r.URL.Path)
	})
	return mux
}

// admit records a request, answers an injected failure in its place,
// and, for a host route, refuses a bearer that is no live host token.
func (s *Server) admit(op string, host bool, next func(w http.ResponseWriter, r *http.Request, body []byte)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			refuse(w, http.StatusBadRequest, "bad_request", "the body could not be read")
			return
		}
		s.mu.Lock()
		s.requests = append(s.requests, Recorded{Op: op, Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
		var f *Failure
		if queue := s.failures[op]; len(queue) > 0 {
			head := queue[0]
			f = &head
			if head.Times > 1 {
				queue[0].Times--
			} else {
				s.failures[op] = queue[1:]
			}
		}
		exp, known := s.hostTokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		live := known && s.now().Before(exp)
		s.mu.Unlock()
		if f != nil {
			refuse(w, f.Status, f.Code, "an injected failure")
			return
		}
		if host && !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			refuse(w, http.StatusUnauthorized, "unauthorized", "missing JWT")
			return
		}
		if host && !live {
			// A token the stub never issued, or one that expired, fails
			// verification, as a stranger's or a person's token fails
			// the host check.
			refuse(w, http.StatusUnauthorized, "unauthorized", "the bearer is no live host token")
			return
		}
		next(w, r, body)
	}
}

// token is the client_credentials grant, authenticated with HTTP Basic.
func (s *Server) token(w http.ResponseWriter, r *http.Request, body []byte) {
	id, secret, ok := r.BasicAuth()
	if form, err := url.ParseQuery(string(body)); err != nil || form.Get("grant_type") != "client_credentials" {
		refuse(w, http.StatusBadRequest, "unsupported_grant_type", "only client_credentials is served")
		return
	}
	if !ok || id != s.clientID || secret != s.clientSecret {
		refuse(w, http.StatusUnauthorized, "invalid_client", "the client id or secret is wrong")
		return
	}
	tok := "host_" + random()
	s.mu.Lock()
	ttl := s.hostTTL
	s.hostTokens[tok] = s.now().Add(ttl)
	s.mu.Unlock()
	reply(w, http.StatusOK, map[string]any{"access_token": tok, "token_type": "Bearer", "expires_in": int64(ttl / time.Second)})
}

// refRE bounds the host's id for an agent and for a session.
var refRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// active is the ref's identity that is neither archived nor disabled.
func (s *Server) active(ref string) *Agent {
	for _, a := range s.agents {
		if a.Ref == ref && a.Status == StatusActive {
			return a
		}
	}
	return nil
}

func (s *Server) put(w http.ResponseWriter, r *http.Request, body []byte) {
	ref := r.PathValue("ref")
	if !refRE.MatchString(ref) {
		refuse(w, http.StatusBadRequest, "bad_request", "ref must be 1 to 128 characters of [A-Za-z0-9._:-]")
		return
	}
	var in struct {
		Name      string `json:"name"`
		Owner     *Owner `json:"owner"`
		AppliedBy string `json:"applied_by"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		refuse(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	switch {
	case strings.TrimSpace(in.Name) == "":
		refuse(w, http.StatusBadRequest, "bad_request", "name is required, at most 200 characters")
		return
	case in.Owner == nil || (in.Owner.Type != "user" && in.Owner.Type != "organization") || in.Owner.ID == "":
		refuse(w, http.StatusBadRequest, "invalid_owner", "owner is required")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	status := http.StatusOK
	a := s.active(ref)
	switch {
	case a == nil:
		a = &Agent{Subject: "agent-" + random()[:16], Ref: ref, Name: in.Name, Owner: *in.Owner, AppliedBy: in.AppliedBy, Status: StatusActive, CreatedAt: s.now().UTC()}
		s.agents = append(s.agents, a)
		status = http.StatusCreated
	case a.Owner != *in.Owner:
		refuse(w, http.StatusConflict, "owner_mismatch", "the agent's active identity has another owner")
		return
	default:
		a.Name = in.Name
	}
	reply(w, status, entry(a))
}

func entry(a *Agent) map[string]any {
	return map[string]any{"subject": a.Subject, "ref": a.Ref, "name": a.Name, "owner": a.Owner, "status": a.Status, "created_at": a.CreatedAt}
}

// confirmed refuses a body without permanent: true.
func confirmed(w http.ResponseWriter, body []byte) (map[string]any, bool) {
	var in map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			refuse(w, http.StatusBadRequest, "bad_request", "invalid json")
			return nil, false
		}
	}
	if in["permanent"] != true {
		refuse(w, http.StatusBadRequest, "confirmation_required", "this act is permanent; send permanent: true")
		return nil, false
	}
	return in, true
}

func (s *Server) archive(w http.ResponseWriter, r *http.Request, body []byte) {
	if _, ok := confirmed(w, body); !ok {
		return
	}
	ref := r.PathValue("ref")
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.active(ref); a != nil {
		a.Status = StatusArchived
		reply(w, http.StatusOK, entry(a))
		return
	}
	for _, a := range slices.Backward(s.agents) {
		if a.Ref == ref {
			reply(w, http.StatusOK, entry(a))
			return
		}
	}
	refuse(w, http.StatusNotFound, "unknown_agent", "no agent identity of this host matches")
}

func (s *Server) disable(w http.ResponseWriter, r *http.Request, body []byte) {
	in, ok := confirmed(w, body)
	if !ok {
		return
	}
	subject, _ := in["subject"].(string)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.agents {
		if a.Subject == subject && a.Ref == r.PathValue("ref") {
			a.Status = StatusDisabled
			reply(w, http.StatusOK, entry(a))
			return
		}
	}
	refuse(w, http.StatusNotFound, "unknown_agent", "no agent identity of this host matches")
}

// list answers the host's identities of one status, in one page.
func (s *Server) list(w http.ResponseWriter, r *http.Request, _ []byte) {
	status := r.URL.Query().Get("status")
	s.mu.Lock()
	defer s.mu.Unlock()
	items := []map[string]any{}
	for _, a := range s.agents {
		if status == "" || a.Status == status {
			items = append(items, entry(a))
		}
	}
	reply(w, http.StatusOK, map[string]any{"items": items, "next_cursor": ""})
}

// mint is the hosted branch of the actor-token route, with the host
// contract's checks in their order.
func (s *Server) mint(w http.ResponseWriter, _ *http.Request, body []byte) {
	var in struct {
		Audience   string          `json:"audience"`
		Subject    string          `json:"subject"`
		Session    json.RawMessage `json:"session"`
		TTLSeconds int64           `json:"ttl_seconds"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		refuse(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	var sess struct {
		ID       string `json:"id"`
		Workload string `json:"workload"`
	}
	dec := json.NewDecoder(strings.NewReader(string(in.Session)))
	dec.DisallowUnknownFields()
	if len(in.Session) == 0 || dec.Decode(&sess) != nil || !refRE.MatchString(sess.ID) || (sess.Workload != "session" && sess.Workload != "sandbox") {
		refuse(w, http.StatusBadRequest, "invalid_session", `session must be {"id", "workload"}`)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var a *Agent
	for _, c := range s.agents {
		if c.Subject == in.Subject {
			a = c
		}
	}
	switch {
	case a == nil:
		refuse(w, http.StatusNotFound, "unknown_agent", "no agent identity of this host matches")
		return
	case a.Status == StatusDisabled:
		refuse(w, http.StatusForbidden, "agent_disabled", "the agent identity is disabled for good")
		return
	case !slices.Contains(s.audiences, in.Audience):
		refuse(w, http.StatusBadRequest, "invalid_target", "audience "+in.Audience+" is not one this host may mint for")
		return
	}
	ttl := time.Duration(in.TTLSeconds) * time.Second
	if ttl <= 0 || ttl > MaxLifetime {
		ttl = MaxLifetime
	}
	tok := "agent_token_" + random()
	s.minted[tok] = Minted{Token: tok, Subject: a.Subject, Ref: a.Ref, Audience: in.Audience, SessionID: sess.ID, Workload: sess.Workload, ExpiresAt: s.now().Add(ttl)}
	reply(w, http.StatusOK, map[string]any{"actor_token": tok, "expires_in": int64(ttl / time.Second)})
}

func random() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("idpstub: no randomness: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func refuse(w http.ResponseWriter, status int, code, msg string) {
	reply(w, status, map[string]string{"error": code, "message": msg})
}

func reply(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
