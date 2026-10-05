// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package cellastub is the stub Cella of spec 026: an HTTP server on
// loopback that serves the routes machine/cella calls, the sandboxes'
// create, read, start, stop and delete, the synchronous exec route, the
// exec socket, the file routes and the tar routes, and the secret read
// and apply.
// Each sandbox is a temporary directory with its workspace in it, and a
// command runs on the machine the test runs on, in that directory, so
// the workspace path a sandbox reports is a real path and a command and
// a file route see the same files. It injects refusals, asks a test's
// authorizer what cellad asks its own, holds a sandbox in Starting, stops
// one as Cella's idle stop does, and loses one. It is a test artifact.
package cellastub

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/authorizer"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/pkg/httpjson"
)

// DefaultEnvironment is the Environment a sandbox that names none is
// placed on.
const DefaultEnvironment = "default"

// DefaultImage is the image a sandbox that names none runs.
const DefaultImage = "base"

// Owner is the owner of every object the stub holds: the subject of its
// one caller, whatever bearer it carries.
const Owner = "stub"

// The operations a Failure is injected on.
const (
	OpCreate  = "create"
	OpGet     = "get"
	OpDelete  = "delete"
	OpStart   = "start"
	OpStop    = "stop"
	OpExec    = "exec"
	OpSession = "session"
	OpFiles   = "files"
	OpSecret  = "secret"
)

// Failure is a refusal answered in place of an operation.
type Failure struct {
	Status int
	Code   string
	Detail string
	// Times is how many requests are refused; zero is once.
	Times int
}

// Recorded is one request the stub received.
type Recorded struct {
	Op     string
	Method string
	Path   string
	Query  url.Values
	Header http.Header
}

// Resource is an object as cellad asks its authorizer about it: the kind,
// the id, and the name, owner and labels cellad renders. An object being
// created has no id yet and the owner it will have. A secret's update
// carries the stored secret alone, as cellad v0.9 sends it, and nothing of
// what the apply writes.
type Resource struct {
	Kind, ID, Name, Owner string
	Labels                map[string]string
}

// Decider is an authorizer: the reason it refuses action on res, or "" to
// allow it.
type Decider func(action string, res Resource) string

// Server is the stub.
type Server struct {
	srv  *httptest.Server
	root string

	mu        sync.Mutex
	decide    Decider
	token     string
	envs      map[string]bool
	secrets   map[string]v1.Secret
	values    map[string]string
	sandboxes map[string]*sandbox
	failures  map[string][]Failure
	starting  int
	requests  []Recorded
	errs      []error
	seq       int
}

// sandbox is one sandbox: its object and its directory, which holds the
// workspace, a home directory and a temporary directory.
type sandbox struct {
	obj      v1.Sandbox
	dir      string
	starting int
}

func (sb *sandbox) workspace() string { return sb.obj.Spec.Workspace.Path }

// New starts a stub for the test and closes it when the test ends.
func New(t testing.TB) *Server {
	// The root is named with its links resolved, so the paths a sandbox
	// reports are the ones a command's pwd -P prints.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		root:      root,
		envs:      map[string]bool{DefaultEnvironment: true},
		secrets:   map[string]v1.Secret{},
		values:    map[string]string{},
		sandboxes: map[string]*sandbox{},
		failures:  map[string][]Failure{},
	}
	s.srv = httptest.NewServer(s.routes())
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the stub's base URL, Cella's API address.
func (s *Server) URL() string { return s.srv.URL }

// RequireToken refuses every request that does not carry the bearer.
func (s *Server) RequireToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
}

// Authorize asks d what cellad asks its authorizer on the routes that
// create a sandbox and read and apply a secret: sandbox.create with the
// manifest's name and labels, secret.mount for each secret it mounts,
// secret.read, and secret.create or secret.update. A refused action
// answers forbidden, and a refused mount not_found, as Cella's do. Nil,
// the default, allows every action.
func (s *Server) Authorize(d Decider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decide = d
}

// refused is the reason the authorizer refuses action on res, or "". The
// caller holds mu.
func (s *Server) refused(action string, res Resource) string {
	if s.decide == nil {
		return ""
	}
	return s.decide(action, res)
}

// secretResource is a secret as the authorizer is asked about it.
func secretResource(sec v1.Secret) Resource {
	return Resource{Kind: authorizer.KindSecret, ID: sec.Status.ID, Name: sec.Metadata.Name, Owner: sec.Status.Owner, Labels: sec.Metadata.Labels}
}

// AddEnvironment makes an Environment known; a sandbox on an unknown one
// is refused with not_found, as Cella refuses it.
func (s *Server) AddEnvironment(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.envs[name] = true
}

// AddSecret stores a secret scoped to hosts.
func (s *Server) AddSecret(name string, hosts ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secrets[name] = v1.Secret{
		APIVersion: v1.APIVersion, Kind: v1.KindSecret, Metadata: v1.Metadata{Name: name},
		Spec:   v1.SecretSpec{Kind: v1.SecretStatic, Scope: v1.SecretScope{Hosts: hosts}},
		Status: v1.SecretStatus{ID: v1.SecretIDPrefix + name, Owner: Owner},
	}
}

// Secret is a secret the stub holds and its value, which no route
// answers; ok is false for a secret it does not hold.
func (s *Server) Secret(name string) (sec v1.Secret, value string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sec, ok = s.secrets[name]
	return sec, s.values[name], ok
}

// Fail refuses the next requests of an operation with f.
func (s *Server) Fail(op string, f Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[op] = append(s.failures[op], f)
}

// StartAfter makes every sandbox created or started from now report
// Starting for that many reads before it runs.
func (s *Server) StartAfter(reads int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starting = reads
}

// Sandbox returns a sandbox by id or name.
func (s *Server) Sandbox(ref string) (v1.Sandbox, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb := s.find(ref)
	if sb == nil {
		return v1.Sandbox{}, false
	}
	return sb.obj, true
}

// Workspace is a sandbox's workspace directory, empty for none.
func (s *Server) Workspace(ref string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sb := s.find(ref); sb != nil {
		return sb.workspace()
	}
	return ""
}

// Stop stops a running sandbox as Cella's idle stop does, keeping its
// workspace.
func (s *Server) Stop(ref string) bool { return s.setPhase(ref, "Stopped", "Idle") }

// SetFailed fails a sandbox with a reason.
func (s *Server) SetFailed(ref, reason string) bool { return s.setPhase(ref, "Failed", reason) }

// Remove deletes a sandbox behind its user's back, as a sandbox lost to
// an operator or a ttl is.
func (s *Server) Remove(ref string) bool {
	s.mu.Lock()
	sb := s.find(ref)
	if sb != nil {
		delete(s.sandboxes, sb.obj.Status.ID)
	}
	s.mu.Unlock()
	return sb != nil
}

func (s *Server) setPhase(ref, phase, reason string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb := s.find(ref)
	if sb == nil {
		return false
	}
	sb.obj.Status.Phase, sb.obj.Status.Reason = phase, reason
	return true
}

// Requests returns every request so far.
func (s *Server) Requests() []Recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// record keeps a failure the stub met while serving that no response
// could carry any more, such as a frame to a client already gone.
func (s *Server) record(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs = append(s.errs, err)
}

// Errors returns the failures the stub recorded while serving.
func (s *Server) Errors() []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.errs)
}

// Count is how many requests of an operation the stub received.
func (s *Server) Count(op string) int {
	n := 0
	for _, r := range s.Requests() {
		if r.Op == op {
			n++
		}
	}
	return n
}

// find is a sandbox by id or name. The caller holds mu.
func (s *Server) find(ref string) *sandbox {
	if sb, ok := s.sandboxes[ref]; ok {
		return sb
	}
	for _, sb := range s.sandboxes {
		if sb.obj.Metadata.Name == ref {
			return sb
		}
	}
	return nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	handle := func(pattern, op string, h func(http.ResponseWriter, *http.Request)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if s.admit(w, r, op) {
				h(w, r)
			}
		})
	}
	handle("POST /v1/sandboxes", OpCreate, s.create)
	handle("GET /v1/sandboxes/{ref}", OpGet, s.get)
	handle("DELETE /v1/sandboxes/{ref}", OpDelete, s.remove)
	handle("POST /v1/sandboxes/{ref}/start", OpStart, s.start)
	handle("POST /v1/sandboxes/{ref}/stop", OpStop, s.stop)
	handle("POST /v1/sandboxes/{ref}/exec", OpExec, s.execWait)
	handle("GET /v1/sandboxes/{ref}/exec", OpSession, s.execSocket)
	handle("GET /v1/sandboxes/{ref}/files", OpFiles, s.exportTar)
	handle("PUT /v1/sandboxes/{ref}/files", OpFiles, s.filesPut)
	handle("DELETE /v1/sandboxes/{ref}/files", OpFiles, s.fileRemove)
	handle("GET /v1/sandboxes/{ref}/files/content", OpFiles, s.fileContent)
	handle("GET /v1/sandboxes/{ref}/files/stat", OpFiles, s.fileStat)
	handle("GET /v1/sandboxes/{ref}/files/list", OpFiles, s.fileList)
	handle("POST /v1/sandboxes/{ref}/files/mkdir", OpFiles, s.fileMkdir)
	handle("POST /v1/sandboxes/{ref}/files/move", OpFiles, s.fileMove)
	handle("GET /v1/secrets/{ref}", OpSecret, s.secret)
	handle("PUT /v1/secrets/{ref}", OpSecret, s.applySecret)
	handle("DELETE /v1/secrets/{ref}", OpSecret, s.removeSecret)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		refuse(w, http.StatusNotFound, "not_found", "the stub serves no "+r.Method+" "+r.URL.Path)
	})
	return mux
}

// admit records a request, checks its bearer, and answers an injected
// failure in its place.
func (s *Server) admit(w http.ResponseWriter, r *http.Request, op string) bool {
	s.mu.Lock()
	s.requests = append(s.requests, Recorded{Op: op, Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone()})
	token := s.token
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
	s.mu.Unlock()
	if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
		refuse(w, http.StatusUnauthorized, "unauthenticated", "the bearer is not the stub's")
		return false
	}
	if f != nil {
		refuse(w, f.Status, f.Code, f.Detail)
		return false
	}
	return true
}

// messages are the fixed user sentences of Cella's error table for the
// codes the stub answers.
var messages = map[string]string{
	"unauthenticated":        "Sign in and send a valid token.",
	"forbidden":              "You do not have permission to do this.",
	"not_found":              "There is no such object.",
	"name_taken":             "You already have an object with this name.",
	"phase_conflict":         "The sandbox is not in a state that allows this.",
	"invalid_field":          "A field has a value it cannot take.",
	"unknown_field":          "The manifest has a field this schema does not know.",
	"bad_request":            "The request could not be read.",
	"exclusive_fields":       "Two fields that cannot be set together are set.",
	"unsupported_media_type": "Send the body in a media type this route accepts.",
	"capability_unsupported": "The environment cannot provide this.",
	"admission_refused":      "The request was refused by this server's policy.",
	"driver_unavailable":     "The environment is unavailable; retry shortly.",
	"quota_exceeded":         "You have reached your limit of running sandboxes. Stop one to start another.",
}

// refuse writes Cella's error envelope.
func refuse(w http.ResponseWriter, status int, code, detail string) {
	e := httpjson.Error{Code: code, Message: messages[code], Details: map[string]any{"request_id": "req_stub"}}
	if e.Message == "" {
		e.Message = http.StatusText(status)
	}
	if detail != "" {
		e.Details["detail"] = detail
	}
	httpjson.WriteError(w, status, e)
}

// namePattern is Cella's rule for a name: a DNS label.
var namePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var obj v1.Sandbox
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&obj); err != nil {
		refuse(w, http.StatusBadRequest, "unknown_field", err.Error())
		return
	}
	if obj.APIVersion != v1.APIVersion || obj.Kind != v1.KindSandbox {
		refuse(w, http.StatusBadRequest, "invalid_field", "the manifest is not a "+v1.APIVersion+" Sandbox")
		return
	}
	name := obj.Metadata.Name
	if len(name) > 63 || !namePattern.MatchString(name) {
		refuse(w, http.StatusBadRequest, "invalid_field", "metadata.name must be a DNS label")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	env := obj.Spec.Environment
	if env == "" {
		env = DefaultEnvironment
	}
	if !s.envs[env] {
		refuse(w, http.StatusNotFound, "not_found", "no environment named "+env)
		return
	}
	if reason := s.refused(authorizer.ActionSandboxCreate, Resource{Kind: authorizer.KindSandbox, Name: name, Owner: Owner, Labels: obj.Metadata.Labels}); reason != "" {
		refuse(w, http.StatusForbidden, "forbidden", authorizer.ActionSandboxCreate+": "+reason)
		return
	}
	for _, m := range obj.Spec.Secrets {
		sec, ok := s.secrets[m.Name]
		if ok && s.refused(authorizer.ActionSecretMount, secretResource(sec)) != "" {
			ok = false
		}
		if !ok {
			refuse(w, http.StatusNotFound, "not_found", "no secret named "+m.Name)
			return
		}
	}
	if s.find(name) != nil {
		refuse(w, http.StatusConflict, "name_taken", "a sandbox is named "+name)
		return
	}
	s.seq++
	id := fmt.Sprintf("sbx_%026d", s.seq)
	sb := &sandbox{obj: obj, dir: filepath.Join(s.root, id), starting: s.starting}
	for _, d := range []string{"workspace", "home", "tmp"} {
		if err := os.MkdirAll(filepath.Join(sb.dir, d), 0o755); err != nil {
			refuse(w, http.StatusServiceUnavailable, "driver_unavailable", err.Error())
			return
		}
	}
	// A sandbox's workspace is its directory's, whatever the manifest
	// asked, as Cella's local driver rewrites it.
	sb.obj.Spec.Workspace.Path = filepath.ToSlash(filepath.Join(sb.dir, "workspace"))
	sb.obj.Spec.Environment = env
	if sb.obj.Spec.Image == "" {
		sb.obj.Spec.Image = DefaultImage
	}
	if sb.obj.Spec.Workdir == "" {
		sb.obj.Spec.Workdir = sb.workspace()
	}
	now := time.Now().UTC()
	sb.obj.Status = v1.SandboxStatus{ID: id, Owner: Owner, Environment: env, Driver: "stub", Isolation: v1.IsolationNone, Phase: "Running", CreatedAt: now, StartedAt: now}
	if sb.starting > 0 {
		sb.obj.Status.Phase = "Starting"
	}
	s.sandboxes[id] = sb
	w.Header().Set("Location", "/v1/sandboxes/"+id)
	httpjson.Write(w, http.StatusCreated, sb.obj)
}

// item finds the route's sandbox, or refuses with not_found.
func (s *Server) item(w http.ResponseWriter, r *http.Request) *sandbox {
	sb := s.find(r.PathValue("ref"))
	if sb == nil {
		refuse(w, http.StatusNotFound, "not_found", "no sandbox "+r.PathValue("ref"))
	}
	return sb
}

// running is a sandbox that runs, as every exec and file route needs, or
// a refusal. It returns a copy of what the route reads.
func (s *Server) running(w http.ResponseWriter, r *http.Request) (sandbox, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb := s.item(w, r)
	if sb == nil {
		return sandbox{}, false
	}
	if sb.obj.Status.Phase != "Running" {
		refuse(w, http.StatusConflict, "phase_conflict", "the sandbox is "+sb.obj.Status.Phase)
		return sandbox{}, false
	}
	sb.obj.Status.LastActivityAt = time.Now().UTC()
	return sandbox{obj: sb.obj, dir: sb.dir}, true
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb := s.item(w, r)
	if sb == nil {
		return
	}
	if sb.starting > 0 {
		sb.starting--
		if sb.starting == 0 && sb.obj.Status.Phase == "Starting" {
			sb.obj.Status.Phase = "Running"
		}
	}
	httpjson.Write(w, http.StatusOK, sb.obj)
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	sb := s.item(w, r)
	if sb != nil {
		delete(s.sandboxes, sb.obj.Status.ID)
	}
	s.mu.Unlock()
	if sb == nil {
		return
	}
	if err := os.RemoveAll(sb.dir); err != nil {
		refuse(w, http.StatusServiceUnavailable, "driver_unavailable", err.Error())
		return
	}
	sb.obj.Status.Phase = "Deleting"
	httpjson.Write(w, http.StatusAccepted, sb.obj)
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb := s.item(w, r)
	if sb == nil {
		return
	}
	if sb.obj.Status.Phase != "Stopped" {
		refuse(w, http.StatusConflict, "phase_conflict", "the sandbox is "+sb.obj.Status.Phase)
		return
	}
	sb.obj.Status.Phase, sb.obj.Status.Reason = "Running", ""
	sb.starting = s.starting
	if sb.starting > 0 {
		sb.obj.Status.Phase = "Starting"
	}
	sb.obj.Status.StartedAt = time.Now().UTC()
	httpjson.Write(w, http.StatusOK, sb.obj)
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb := s.item(w, r)
	if sb == nil {
		return
	}
	if sb.obj.Status.Phase != "Running" {
		refuse(w, http.StatusConflict, "phase_conflict", "the sandbox is "+sb.obj.Status.Phase)
		return
	}
	sb.obj.Status.Phase = "Stopped"
	sb.obj.Status.StoppedAt = time.Now().UTC()
	httpjson.Write(w, http.StatusOK, sb.obj)
}

func (s *Server) secret(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sec, ok := s.secrets[r.PathValue("ref")]
	if !ok {
		refuse(w, http.StatusNotFound, "not_found", "no secret "+r.PathValue("ref"))
		return
	}
	if reason := s.refused(authorizer.ActionSecretRead, secretResource(sec)); reason != "" {
		refuse(w, http.StatusForbidden, "forbidden", authorizer.ActionSecretRead+": "+reason)
		return
	}
	httpjson.Write(w, http.StatusOK, sec)
}

// applySecret creates or updates a secret by name, keeping its value
// and counting value writes in its version, as Cella does; the answer
// carries no value. The authorizer is asked secret.create about the
// secret the apply makes, and secret.update about the one it replaces.
func (s *Server) applySecret(w http.ResponseWriter, r *http.Request) {
	var in v1.Secret
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		refuse(w, http.StatusBadRequest, "bad_request", "the secret does not decode: "+err.Error())
		return
	}
	name := r.PathValue("ref")
	switch {
	case in.Kind != v1.KindSecret || in.Metadata.Name != name:
		refuse(w, http.StatusBadRequest, "invalid_field", "the body is not a Secret named "+name)
		return
	case len(in.Spec.Scope.Hosts) == 0:
		refuse(w, http.StatusBadRequest, "invalid_field", "a secret names the hosts it is for")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, held := s.secrets[name]
	value := in.Spec.Value
	in.Spec.Value = ""
	in.Status = prev.Status
	if !held {
		in.Status = v1.SecretStatus{ID: v1.SecretIDPrefix + name, Owner: Owner, CreatedAt: time.Now().UTC()}
	}
	action, asked := authorizer.ActionSecretUpdate, prev
	if !held {
		action, asked = authorizer.ActionSecretCreate, in
		asked.Status.ID = ""
	}
	if reason := s.refused(action, secretResource(asked)); reason != "" {
		refuse(w, http.StatusForbidden, "forbidden", action+": "+reason)
		return
	}
	if value != "" {
		s.values[name] = value
		in.Status.Version++
	}
	s.secrets[name] = in
	status := http.StatusOK
	if !held {
		status = http.StatusCreated
	}
	httpjson.Write(w, status, in)
}

// removeSecret deletes a secret by name and its value, asking the
// authorizer secret.delete about the stored secret, as Cella does.
func (s *Server) removeSecret(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := r.PathValue("ref")
	sec, ok := s.secrets[name]
	if !ok {
		refuse(w, http.StatusNotFound, "not_found", "no secret "+name)
		return
	}
	if reason := s.refused(authorizer.ActionSecretDelete, secretResource(sec)); reason != "" {
		refuse(w, http.StatusForbidden, "forbidden", authorizer.ActionSecretDelete+": "+reason)
		return
	}
	delete(s.secrets, name)
	delete(s.values, name)
	httpjson.Write(w, http.StatusOK, sec)
}

// sandboxPath is the PATH of every command, as an image's would be.
const sandboxPath = "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

// environ is a command's environment: the sandbox's own, a placeholder
// for each mounted secret, and the request's additions.
func environ(sb sandbox, add map[string]string) []string {
	env := map[string]string{
		"PATH":   sandboxPath,
		"HOME":   filepath.Join(sb.dir, "home"),
		"TMPDIR": filepath.Join(sb.dir, "tmp"),
		"LANG":   "C",
	}
	maps.Copy(env, sb.obj.Spec.Env)
	for _, m := range sb.obj.Spec.Secrets {
		env[m.Env] = "cella-placeholder-" + m.Name
	}
	maps.Copy(env, add)
	out := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, k+"="+env[k])
	}
	return out
}

// workdir is where a command starts: the request's, or the sandbox's.
func workdir(sb sandbox, dir string) string {
	if dir != "" {
		return dir
	}
	return sb.obj.Spec.Workdir
}

// lookPath finds a command's program on the sandbox's PATH.
func lookPath(name string) string {
	if strings.Contains(name, "/") {
		return name
	}
	for dir := range strings.SplitSeq(sandboxPath, ":") {
		p := path.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return name
}

// execTimeout reads a command's timeout: positive and at most an hour,
// ten minutes when absent, as Cella reads it.
func execTimeout(s string) (time.Duration, error) {
	if s == "" {
		return 10 * time.Minute, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 || d > time.Hour {
		return 0, errors.New("timeout must be positive and at most 1h")
	}
	return d, nil
}
