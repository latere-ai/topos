// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package arcastub is a stub of an Arca-compatible files API, the
// backend of a session's files store (spec 068): an HTTP server on
// loopback that serves the routes a files store calls under its root,
// "/v1": a put of one object, a read, a folder copy, and the three
// routes of an upload session, with a second server standing for the
// bucket, which answers a read's redirect and takes the parts of an
// upload. It holds the contract a client must keep: a put without
// Content-Length is 411, an If-None-Match: * onto a taken path is 412,
// a put above the single-request bound is 413, and a request to the
// bucket that carries an Authorization header is refused and reported to
// the test. Each request is checked against the token a test names, and
// recorded. It is a test artifact.
package arcastub

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The operations a Failure is injected on.
const (
	OpPut      = "put"
	OpRead     = "read"
	OpCopy     = "copy"
	OpUpload   = "upload"
	OpPart     = "part"
	OpComplete = "complete"
)

// Failure is a refusal answered in place of an operation.
type Failure struct {
	Status int
	Code   string
	// Times is how many requests are refused; zero is once.
	Times int
}

// Recorded is one request the stub or its bucket received.
type Recorded struct {
	Op     string
	Method string
	Path   string
	Header http.Header
}

// Object is one object the stub holds.
type Object struct {
	Data        []byte
	ContentType string
	// Token is the bearer of the request that wrote it.
	Token string
}

// upload is an open upload session.
type upload struct {
	owner, path, contentType, token string
	size                            int64
	parts                           map[int][]byte
}

// Server is the stub.
type Server struct {
	t      testing.TB
	srv    *httptest.Server
	bucket *httptest.Server

	mu       sync.Mutex
	objects  map[string]map[string]Object
	uploads  map[string]*upload
	failures map[string][]Failure
	requests []Recorded
	tokenOK  func(string) bool
	// inline is the single-request bound; redirectAbove the size above
	// which a read is a redirect to the bucket; partSize an upload's
	// part size.
	inline, redirectAbove, partSize int64
}

// New starts a stub and its bucket, closed when the test ends. Every
// bearer is accepted until RequireToken names a check; a read of any size
// redirects until RedirectAbove sets a bound.
func New(t testing.TB) *Server {
	s := &Server{t: t, objects: map[string]map[string]Object{}, uploads: map[string]*upload{}, failures: map[string][]Failure{},
		tokenOK: func(string) bool { return true }, inline: 16 << 20, redirectAbove: -1, partSize: 16 << 20}
	api := http.NewServeMux()
	api.HandleFunc("PUT /v1/files/{owner}/{path...}", s.put)
	api.HandleFunc("GET /v1/files/{owner}/{path...}", s.read)
	api.HandleFunc("POST /v1/files/{owner}/{path...}", s.copy)
	api.HandleFunc("POST /v1/uploads", s.open)
	api.HandleFunc("POST /v1/uploads/{id}/complete", s.complete)
	api.HandleFunc("DELETE /v1/uploads/{id}", s.abort)
	s.srv = httptest.NewServer(api)
	bucket := http.NewServeMux()
	bucket.HandleFunc("GET /objects/{key}", s.object)
	bucket.HandleFunc("PUT /parts/{id}/{n}", s.part)
	s.bucket = httptest.NewServer(bucket)
	t.Cleanup(func() {
		s.srv.Close()
		s.bucket.Close()
	})
	return s
}

// URL is the files API's root, under which files/ and uploads are served.
func (s *Server) URL() string { return s.srv.URL + "/v1" }

// RequireToken checks every bearer of the files API with ok; a request
// whose bearer fails it is 401.
func (s *Server) RequireToken(ok func(token string) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenOK = ok
}

// SetInline sets the single-request bound, past which a put is 413.
func (s *Server) SetInline(n int64) { s.mu.Lock(); s.inline = n; s.mu.Unlock() }

// RedirectAbove makes a read of an object of at most n bytes answer its
// bytes, and a larger one redirect to the bucket; a negative n redirects
// every read.
func (s *Server) RedirectAbove(n int64) { s.mu.Lock(); s.redirectAbove = n; s.mu.Unlock() }

// SetPartSize sets the part size of an upload session.
func (s *Server) SetPartSize(n int64) { s.mu.Lock(); s.partSize = n; s.mu.Unlock() }

// Fail refuses the next requests of op with f.
func (s *Server) Fail(op string, f Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[op] = append(s.failures[op], f)
}

// Requests are the requests received, in order.
func (s *Server) Requests() []Recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// Object is the object at path of owner's space.
func (s *Server) Object(owner, path string) (Object, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[owner][path]
	return o, ok
}

// Paths are the paths of owner's space under prefix, sorted.
func (s *Server) Paths(owner, prefix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for p := range s.objects[owner] {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// Put writes an object as a test sets one up.
func (s *Server) Put(owner, path string, o Object) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objects[owner] == nil {
		s.objects[owner] = map[string]Object{}
	}
	s.objects[owner][path] = o
}

// Remove removes an object as a test takes one away.
func (s *Server) Remove(owner, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects[owner], path)
}

// Uploads is how many upload sessions are open.
func (s *Server) Uploads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.uploads)
}

// refuse writes the files API's error envelope.
func refuse(w http.ResponseWriter, status int, code, detail string) {
	reply(w, status, map[string]any{"error": map[string]any{"code": code, "message": "The stub refused the request.", "details": map[string]any{"detail": detail}}})
}

// reply writes v as JSON with status; a value that does not encode is a
// fault of the stub, answered 500.
func reply(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "the stub's answer does not encode: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// A write to a closed connection is the client's to notice.
	_, _ = w.Write(append(b, '\n'))
}

// begin records r as op and answers false, having refused it, when a
// failure is injected or its bearer does not pass; bucket requests carry
// no bearer, and one that does fails the test.
func (s *Server) begin(w http.ResponseWriter, r *http.Request, op string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, Recorded{Op: op, Method: r.Method, Path: r.URL.EscapedPath(), Header: r.Header.Clone()})
	if fs := s.failures[op]; len(fs) > 0 {
		f := &fs[0]
		if f.Times <= 1 {
			s.failures[op] = fs[1:]
		} else {
			f.Times--
		}
		refuse(w, f.Status, f.Code, "an injected failure")
		return "", false
	}
	auth := r.Header.Get("Authorization")
	if op == OpPart || op == "bucket-read" {
		if auth != "" {
			s.t.Errorf("arcastub: %s %s reached the bucket with an Authorization header", r.Method, r.URL.Path)
			refuse(w, http.StatusBadRequest, "bad_request", "a presigned address takes no Authorization header")
			return "", false
		}
		return "", true
	}
	tok, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || tok == "" || !s.tokenOK(tok) {
		refuse(w, http.StatusUnauthorized, "unauthenticated", "no bearer the stub accepts")
		return "", false
	}
	return tok, true
}

// key is a space and a path as the files API names them.
func key(r *http.Request) (string, string) { return r.PathValue("owner"), r.PathValue("path") }

func (s *Server) put(w http.ResponseWriter, r *http.Request) {
	tok, ok := s.begin(w, r, OpPut)
	if !ok {
		return
	}
	owner, path := key(r)
	if !strings.HasPrefix(path, "files/") {
		refuse(w, http.StatusBadRequest, "unknown_plane", "the path does not begin with files/")
		return
	}
	if r.Header.Get("Content-Length") == "" || r.ContentLength < 0 {
		refuse(w, http.StatusLengthRequired, "length_required", "a put names its length")
		return
	}
	s.mu.Lock()
	inline := s.inline
	_, taken := s.objects[owner][path]
	s.mu.Unlock()
	if r.ContentLength > inline {
		refuse(w, http.StatusRequestEntityTooLarge, "object_too_large", "past the single-request bound; open an upload")
		return
	}
	if taken && r.Header.Get("If-None-Match") == "*" {
		refuse(w, http.StatusPreconditionFailed, "precondition_failed", "the path is taken")
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil || int64(len(data)) != r.ContentLength {
		refuse(w, http.StatusBadRequest, "bad_request", "the body is not its length")
		return
	}
	s.Put(owner, path, Object{Data: data, ContentType: r.Header.Get("Content-Type"), Token: tok})
	sum := sha256.Sum256(data)
	reply(w, http.StatusCreated, map[string]any{"path": path, "size": len(data), "checksum": hex.EncodeToString(sum[:]), "checksum_kind": "sha256"})
}

func (s *Server) read(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.begin(w, r, OpRead); !ok {
		return
	}
	owner, path := key(r)
	o, ok := s.Object(owner, path)
	if !ok {
		refuse(w, http.StatusNotFound, "not_found", "nothing is at the path")
		return
	}
	s.mu.Lock()
	above := s.redirectAbove
	s.mu.Unlock()
	if above < 0 || int64(len(o.Data)) > above {
		k := hex.EncodeToString([]byte(owner + "\x00" + path))
		w.Header().Set("Location", s.bucket.URL+"/objects/"+k)
		w.WriteHeader(http.StatusFound)
		return
	}
	serve(w, o)
}

// serve answers an object's bytes.
func serve(w http.ResponseWriter, o Object) {
	w.Header().Set("Content-Type", o.ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(o.Data)))
	w.WriteHeader(http.StatusOK)
	// A write to a closed connection is the client's to notice.
	_, _ = w.Write(o.Data)
}

func (s *Server) object(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.begin(w, r, "bucket-read"); !ok {
		return
	}
	raw, err := hex.DecodeString(r.PathValue("key"))
	owner, path, found := strings.Cut(string(raw), "\x00")
	if err != nil || !found {
		refuse(w, http.StatusNotFound, "not_found", "no such key")
		return
	}
	o, ok := s.Object(owner, path)
	if !ok {
		refuse(w, http.StatusNotFound, "not_found", "no such key")
		return
	}
	serve(w, o)
}

func (s *Server) copy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.begin(w, r, OpCopy); !ok {
		return
	}
	owner, from := key(r)
	var in struct {
		CopyTo    string `json:"copy_to"`
		Recursive bool   `json:"recursive"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || in.CopyTo == "" || !in.Recursive {
		refuse(w, http.StatusBadRequest, "invalid_field", "the stub copies folders: copy_to and recursive true")
		return
	}
	if in.CopyTo == from || strings.HasPrefix(in.CopyTo, from+"/") {
		refuse(w, http.StatusBadRequest, "invalid_field", "copy_to is inside the source")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	space := s.objects[owner]
	moved := map[string]Object{}
	var bytes int
	for p, o := range space {
		rest, ok := strings.CutPrefix(p, from+"/")
		if !ok {
			continue
		}
		dst := in.CopyTo + "/" + rest
		if _, taken := space[dst]; taken {
			refuse(w, http.StatusConflict, "path_taken", "a live object holds "+dst)
			return
		}
		moved[dst] = o
		bytes += len(o.Data)
	}
	if len(moved) == 0 {
		refuse(w, http.StatusNotFound, "not_found", "nothing is under the folder")
		return
	}
	maps.Copy(space, moved)
	reply(w, http.StatusOK, map[string]any{"path": in.CopyTo, "files": len(moved), "bytes": bytes})
}

func (s *Server) open(w http.ResponseWriter, r *http.Request) {
	tok, ok := s.begin(w, r, OpUpload)
	if !ok {
		return
	}
	var in struct {
		Owner       string `json:"owner"`
		Path        string `json:"path"`
		Size        int64  `json:"size"`
		ContentType string `json:"content_type"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || in.Owner == "" || in.Path == "" || in.Size <= 0 {
		refuse(w, http.StatusBadRequest, "invalid_field", "owner, path and a positive size")
		return
	}
	id := rand.Text()
	s.mu.Lock()
	part := s.partSize
	s.uploads[id] = &upload{owner: in.Owner, path: in.Path, contentType: in.ContentType, token: tok, size: in.Size, parts: map[int][]byte{}}
	s.mu.Unlock()
	count := (in.Size + part - 1) / part
	urls := make([]string, count)
	for i := range urls {
		urls[i] = s.bucket.URL + "/parts/" + id + "/" + strconv.Itoa(i+1)
	}
	reply(w, http.StatusCreated, map[string]any{"id": id, "owner": in.Owner, "path": in.Path, "part_size": part, "part_count": count, "part_urls": urls, "expires_at": "2026-10-13T00:00:00Z"})
}

func (s *Server) part(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.begin(w, r, OpPart); !ok {
		return
	}
	if r.Header.Get("Content-Length") == "" || r.ContentLength < 0 {
		refuse(w, http.StatusLengthRequired, "length_required", "a part names its length")
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	data, rerr := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.uploads[r.PathValue("id")]
	if err != nil || rerr != nil || !ok {
		refuse(w, http.StatusNotFound, "not_found", "no such upload part")
		return
	}
	u.parts[n] = data
	sum := sha256.Sum256(data)
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:8])+`"`)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) complete(w http.ResponseWriter, r *http.Request) {
	tok, ok := s.begin(w, r, OpComplete)
	if !ok {
		return
	}
	var in struct {
		Parts []struct {
			N    int    `json:"n"`
			ETag string `json:"etag"`
		} `json:"parts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		refuse(w, http.StatusBadRequest, "bad_request", "the body is not JSON")
		return
	}
	s.mu.Lock()
	u, found := s.uploads[r.PathValue("id")]
	s.mu.Unlock()
	if !found || u.token != tok {
		refuse(w, http.StatusNotFound, "not_found", "no such upload")
		return
	}
	var data []byte
	for i, p := range in.Parts {
		b, ok := u.parts[p.N]
		sum := sha256.Sum256(b)
		if !ok || p.N != i+1 || strings.Trim(p.ETag, `"`) != hex.EncodeToString(sum[:8]) {
			refuse(w, http.StatusBadRequest, "invalid_field", "a part's number or ETag is wrong")
			return
		}
		data = append(data, b...)
	}
	if int64(len(data)) != u.size {
		refuse(w, http.StatusBadRequest, "invalid_field", "the parts are not the declared size")
		return
	}
	if _, taken := s.Object(u.owner, u.path); taken && r.Header.Get("If-None-Match") == "*" {
		refuse(w, http.StatusPreconditionFailed, "precondition_failed", "the path is taken")
		return
	}
	s.Put(u.owner, u.path, Object{Data: data, ContentType: u.contentType, Token: tok})
	s.mu.Lock()
	delete(s.uploads, r.PathValue("id"))
	s.mu.Unlock()
	reply(w, http.StatusCreated, map[string]any{"path": u.path, "size": len(data), "checksum_kind": "etag"})
}

func (s *Server) abort(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.begin(w, r, "abort"); !ok {
		return
	}
	s.mu.Lock()
	delete(s.uploads, r.PathValue("id"))
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
