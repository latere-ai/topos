// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arcastub

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// do sends one request and answers its status and body.
func do(t *testing.T, method, url string, body []byte, header map[string]string) (int, http.Header, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if header["chunked"] != "" {
		req.ContentLength = -1
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, b
}

func code(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("not an error envelope: %s", body)
	}
	return env.Error.Code
}

// TestTheStubHoldsTheContract: the stub refuses a bearer it does not
// accept, a put with no length, a put past its bound, a create-only put
// onto a taken path and a path outside files/; it reads by redirect or
// inline, copies a folder, and assembles an upload from its parts, whose
// requests carry no bearer; and it answers an injected failure.
func TestTheStubHoldsTheContract(t *testing.T) {
	s := New(t)
	s.RequireToken(func(tok string) bool { return tok == "ok" })
	bearer := map[string]string{"Authorization": "Bearer ok"}
	file := s.URL() + "/files/o/files/a/b.txt"
	if c, _, b := do(t, http.MethodPut, file, []byte("x"), map[string]string{"Authorization": "Bearer no"}); c != http.StatusUnauthorized || code(t, b) != "unauthenticated" {
		t.Fatalf("a bad bearer: %d %s", c, b)
	}
	if c, _, b := do(t, http.MethodPut, file, []byte("x"), map[string]string{"Authorization": "Bearer ok", "chunked": "1"}); c != http.StatusLengthRequired {
		t.Fatalf("a put with no length: %d %s", c, b)
	}
	if c, _, b := do(t, http.MethodPut, s.URL()+"/files/o/other/b.txt", []byte("x"), bearer); c != http.StatusBadRequest || code(t, b) != "unknown_plane" {
		t.Fatalf("a put outside files/: %d %s", c, b)
	}
	s.SetInline(4)
	if c, _, b := do(t, http.MethodPut, file, []byte("12345"), bearer); c != http.StatusRequestEntityTooLarge || code(t, b) != "object_too_large" {
		t.Fatalf("a put past the bound: %d %s", c, b)
	}
	s.SetInline(16 << 20)
	if c, _, b := do(t, http.MethodPut, file, []byte("hello"), map[string]string{"Authorization": "Bearer ok", "If-None-Match": "*", "Content-Type": "text/plain"}); c != http.StatusCreated {
		t.Fatalf("a put: %d %s", c, b)
	}
	if c, _, b := do(t, http.MethodPut, file, []byte("again"), map[string]string{"Authorization": "Bearer ok", "If-None-Match": "*"}); c != http.StatusPreconditionFailed {
		t.Fatalf("a create-only put onto a taken path: %d %s", c, b)
	}
	c, h, _ := do(t, http.MethodGet, file, nil, bearer)
	if c != http.StatusFound || h.Get("Location") == "" {
		t.Fatalf("a read: %d %v", c, h)
	}
	if c, _, b := do(t, http.MethodGet, h.Get("Location"), nil, nil); c != http.StatusOK || string(b) != "hello" {
		t.Fatalf("the bucket's read: %d %s", c, b)
	}
	if c, _, b := do(t, http.MethodGet, s.bucket.URL+"/objects/zz", nil, nil); c != http.StatusNotFound {
		t.Fatalf("a bucket read of no key: %d %s", c, b)
	}
	if c, _, b := do(t, http.MethodGet, s.bucket.URL+"/objects/6f00", nil, nil); c != http.StatusNotFound {
		t.Fatalf("a bucket read of a gone key: %d %s", c, b)
	}
	s.RedirectAbove(100)
	if c, _, b := do(t, http.MethodGet, file, nil, bearer); c != http.StatusOK || string(b) != "hello" {
		t.Fatalf("an inline read: %d %s", c, b)
	}
	if c, _, _ := do(t, http.MethodGet, s.URL()+"/files/o/files/none", nil, bearer); c != http.StatusNotFound {
		t.Fatalf("a read of nothing: %d", c)
	}
	copyTo := func(body string) (int, []byte) {
		c, _, b := do(t, http.MethodPost, s.URL()+"/files/o/files/a", []byte(body), bearer)
		return c, b
	}
	if c, b := copyTo(`{"copy_to":"files/c","recursive":true}`); c != http.StatusOK || !strings.Contains(string(b), `"files":1`) {
		t.Fatalf("a copy: %d %s", c, b)
	}
	if o, ok := s.Object("o", "files/c/b.txt"); !ok || string(o.Data) != "hello" || len(s.Paths("o", "files/")) != 2 {
		t.Fatalf("the copied object %+v %t %v", o, ok, s.Paths("o", "files/"))
	}
	for body, want := range map[string]int{`{"copy_to":"files/c","recursive":true}`: http.StatusConflict, `{"copy_to":"files/a/x","recursive":true}`: http.StatusBadRequest, `{"copy_to":"files/d"}`: http.StatusBadRequest, `{`: http.StatusBadRequest} {
		if c, b := copyTo(body); c != want {
			t.Fatalf("a copy of %s: %d %s", body, c, b)
		}
	}
	if c, _, _ := do(t, http.MethodPost, s.URL()+"/files/o/files/none", []byte(`{"copy_to":"files/e","recursive":true}`), bearer); c != http.StatusNotFound {
		t.Fatalf("a copy of nothing: %d", c)
	}

	s.SetPartSize(3)
	if c, _, b := do(t, http.MethodPost, s.URL()+"/uploads", []byte(`{"owner":"o"}`), bearer); c != http.StatusBadRequest {
		t.Fatalf("an upload with no path: %d %s", c, b)
	}
	c, _, b := do(t, http.MethodPost, s.URL()+"/uploads", []byte(`{"owner":"o","path":"files/u.bin","size":5,"content_type":"application/octet-stream"}`), bearer)
	var up struct {
		ID       string   `json:"id"`
		PartURLs []string `json:"part_urls"`
	}
	if c != http.StatusCreated || json.Unmarshal(b, &up) != nil || len(up.PartURLs) != 2 || s.Uploads() != 1 {
		t.Fatalf("an upload: %d %s", c, b)
	}
	var etags []string
	for i, chunk := range []string{"abc", "de"} {
		if c, _, b := do(t, http.MethodPut, up.PartURLs[i], []byte(chunk), map[string]string{"chunked": "1"}); c != http.StatusLengthRequired {
			t.Fatalf("a part with no length: %d %s", c, b)
		}
		c, h, b := do(t, http.MethodPut, up.PartURLs[i], []byte(chunk), nil)
		if c != http.StatusOK || h.Get("ETag") == "" {
			t.Fatalf("a part: %d %s", c, b)
		}
		etags = append(etags, h.Get("ETag"))
	}
	if c, _, _ := do(t, http.MethodPut, s.bucket.URL+"/parts/none/1", []byte("x"), nil); c != http.StatusNotFound {
		t.Fatalf("a part of no upload: %d", c)
	}
	complete := s.URL() + "/uploads/" + up.ID + "/complete"
	if c, _, _ := do(t, http.MethodPost, complete, []byte(`{"parts":[{"n":1,"etag":"x"}]}`), bearer); c != http.StatusBadRequest {
		t.Fatalf("a completion with a wrong ETag: %d", c)
	}
	if c, _, _ := do(t, http.MethodPost, complete, []byte(`{"parts":[{"n":1,"etag":`+etags[0][:0]+`"`+strings.Trim(etags[0], `"`)+`"}]}`), bearer); c != http.StatusBadRequest {
		t.Fatalf("a completion short of the size: %d", c)
	}
	if c, _, _ := do(t, http.MethodPost, complete, []byte(`{`), bearer); c != http.StatusBadRequest {
		t.Fatalf("a completion that is not JSON: %d", c)
	}
	parts, err := json.Marshal(map[string]any{"parts": []map[string]any{{"n": 1, "etag": etags[0]}, {"n": 2, "etag": etags[1]}}})
	if err != nil {
		t.Fatal(err)
	}
	if c, _, b := do(t, http.MethodPost, complete, parts, bearer); c != http.StatusCreated || s.Uploads() != 0 {
		t.Fatalf("a completion: %d %s", c, b)
	}
	if o, _ := s.Object("o", "files/u.bin"); string(o.Data) != "abcde" {
		t.Fatalf("the assembled object %q", o.Data)
	}
	if c, _, _ := do(t, http.MethodPost, complete, parts, bearer); c != http.StatusNotFound {
		t.Fatalf("a completion of a closed upload: %d", c)
	}
	_, _, b = do(t, http.MethodPost, s.URL()+"/uploads", []byte(`{"owner":"o","path":"files/u.bin","size":2}`), bearer)
	if json.Unmarshal(b, &up) != nil {
		t.Fatal(string(b))
	}
	_, h, _ = do(t, http.MethodPut, up.PartURLs[0], []byte("zz"), nil)
	again, err := json.Marshal(map[string]any{"parts": []map[string]any{{"n": 1, "etag": h.Get("ETag")}}})
	if err != nil {
		t.Fatal(err)
	}
	if c, _, _ := do(t, http.MethodPost, s.URL()+"/uploads/"+up.ID+"/complete", again, map[string]string{"Authorization": "Bearer ok", "If-None-Match": "*"}); c != http.StatusPreconditionFailed {
		t.Fatalf("a create-only completion onto a taken path: %d", c)
	}
	if c, _, _ := do(t, http.MethodDelete, s.URL()+"/uploads/"+up.ID, nil, bearer); c != http.StatusNoContent || s.Uploads() != 0 {
		t.Fatalf("an abort: %d", c)
	}
	s.Fail(OpRead, Failure{Status: http.StatusServiceUnavailable, Code: "storage_unavailable", Times: 2})
	for range 2 {
		if c, _, b := do(t, http.MethodGet, file, nil, bearer); c != http.StatusServiceUnavailable || code(t, b) != "storage_unavailable" {
			t.Fatalf("an injected failure: %d %s", c, b)
		}
	}
	if c, _, _ := do(t, http.MethodGet, file, nil, bearer); c != http.StatusOK {
		t.Fatalf("after the failures: %d", c)
	}
	if len(s.Requests()) == 0 {
		t.Fatal("no request was recorded")
	}
}

// TestABearerAtTheBucketFailsTheTest: a request to the bucket that
// carries an Authorization header is refused, and the stub reports it to
// the test.
func TestABearerAtTheBucketFailsTheTest(t *testing.T) {
	rec := &recorder{TB: t}
	s := New(rec)
	s.Put("o", "files/a", Object{Data: []byte("a")})
	_, h, _ := do(t, http.MethodGet, s.URL()+"/files/o/files/a", nil, map[string]string{"Authorization": "Bearer ok"})
	if c, _, _ := do(t, http.MethodGet, h.Get("Location"), nil, map[string]string{"Authorization": "Bearer ok"}); c != http.StatusBadRequest || !rec.failed {
		t.Fatalf("a bearer at the bucket: %d, reported %t", c, rec.failed)
	}
}

// recorder is a test that records an Errorf instead of failing.
type recorder struct {
	testing.TB
	failed bool
}

func (r *recorder) Errorf(string, ...any) { r.failed = true }

// TestATestRemovesAnObject: an object a test removes is gone.
func TestATestRemovesAnObject(t *testing.T) {
	s := New(t)
	s.Put("o", "files/a", Object{Data: []byte("a")})
	s.Remove("o", "files/a")
	if _, ok := s.Object("o", "files/a"); ok {
		t.Fatal("the object is still there")
	}
}

// TestTheStubDeletesForGood: a permanent delete removes one object and a
// second one is 404; a delete that is not permanent, and a recursive one,
// are refused, and the object stays.
func TestTheStubDeletesForGood(t *testing.T) {
	s := New(t)
	bearer := map[string]string{"Authorization": "Bearer ok"}
	s.Put("o", "files/a/b.txt", Object{Data: []byte("b")})
	file := s.URL() + "/files/o/files/a/b.txt"
	for _, q := range []string{"", "?permanent=1&recursive=1"} {
		if c, _, b := do(t, http.MethodDelete, file+q, nil, bearer); c != http.StatusBadRequest {
			t.Fatalf("a delete %q: %d %s", q, c, b)
		}
	}
	if _, ok := s.Object("o", "files/a/b.txt"); !ok {
		t.Fatal("a refused delete removed the object")
	}
	if c, _, b := do(t, http.MethodDelete, file+"?permanent=1", nil, bearer); c != http.StatusNoContent {
		t.Fatalf("a delete: %d %s", c, b)
	}
	if _, ok := s.Object("o", "files/a/b.txt"); ok {
		t.Fatal("the deleted object is still there")
	}
	if c, _, b := do(t, http.MethodDelete, file+"?permanent=1", nil, bearer); c != http.StatusNotFound || code(t, b) != "not_found" {
		t.Fatalf("a delete of nothing: %d %s", c, b)
	}
}

// TestTheStubReadsAVersion: a read of ?version=N answers the version a
// test kept, inline or through the bucket, a version it did not keep is
// 404, and the read is recorded with its query.
func TestTheStubReadsAVersion(t *testing.T) {
	s := New(t)
	bearer := map[string]string{"Authorization": "Bearer ok"}
	s.Put("o", "files/a.txt", Object{Data: []byte("now"), ContentType: "text/plain"})
	s.PutVersion("o", "files/a.txt", 2, Object{Data: []byte("then"), ContentType: "text/plain"})
	file := s.URL() + "/files/o/files/a.txt"
	c, h, b := do(t, http.MethodGet, file+"?version=2", nil, bearer)
	if c != http.StatusFound {
		t.Fatalf("a version past the bound: %d %s", c, b)
	}
	if c, _, b := do(t, http.MethodGet, h.Get("Location"), nil, nil); c != http.StatusOK || string(b) != "then" {
		t.Fatalf("a version through the bucket: %d %s", c, b)
	}
	s.RedirectAbove(1 << 20)
	if c, _, b := do(t, http.MethodGet, file+"?version=2", nil, bearer); c != http.StatusOK || string(b) != "then" {
		t.Fatalf("a version inline: %d %s", c, b)
	}
	if c, _, b := do(t, http.MethodGet, file, nil, bearer); c != http.StatusOK || string(b) != "now" {
		t.Fatalf("the file as it stands: %d %s", c, b)
	}
	for _, v := range []string{"3", "x"} {
		if c, _, b := do(t, http.MethodGet, file+"?version="+v, nil, bearer); c != http.StatusNotFound || code(t, b) != "not_found" {
			t.Fatalf("version %s: %d %s", v, c, b)
		}
	}
	if r := s.Requests(); r[0].Query != "version=2" || r[0].Header.Get("Authorization") != "Bearer ok" {
		t.Fatalf("the first read is recorded as %+v", r[0])
	}
}

// TestTheStubServesMemory: every read, HEAD and put answers the object's
// checksum as its ETag; a HEAD answers headers and never a redirect; a
// listing pages through a folder with the directories a page implies; an
// If-Match that names another version is 412 on a put and a delete; a
// delete that is not permanent moves the object to the trash once the
// test keeps one; and an injected failure carries its message and detail
// (spec 093).
func TestTheStubServesMemory(t *testing.T) {
	s := New(t)
	s.KeepTrash()
	bearer := map[string]string{"Authorization": "Bearer ok"}
	file := s.URL() + "/files/o/files/m/a.md"
	c, h, b := do(t, http.MethodPut, file, []byte("a"), map[string]string{"Authorization": "Bearer ok", "If-None-Match": "*"})
	tag := `"` + checksum([]byte("a")) + `"`
	if c != http.StatusCreated || h.Get("ETag") != tag {
		t.Fatalf("a put: %d %v %s", c, h, b)
	}
	if c, h, _ := do(t, http.MethodHead, file, nil, bearer); c != http.StatusOK || h.Get("ETag") != tag || h.Get("Content-Length") != "1" {
		t.Fatalf("a HEAD: %d %v", c, h)
	}
	if c, h, _ := do(t, http.MethodGet, file, nil, bearer); c != http.StatusFound || h.Get("ETag") != tag {
		t.Fatalf("a read: %d %v", c, h)
	}
	if c, _, _ := do(t, http.MethodHead, s.URL()+"/files/o/files/m/none.md", nil, bearer); c != http.StatusNotFound {
		t.Fatalf("a HEAD of nothing: %d", c)
	}
	for name, h := range map[string]string{"another version": `"0000"`, "nothing there": tag} {
		target := file
		if name == "nothing there" {
			target = s.URL() + "/files/o/files/m/none.md"
		}
		if c, _, b := do(t, http.MethodPut, target, []byte("b"), map[string]string{"Authorization": "Bearer ok", "If-Match": h}); c != http.StatusPreconditionFailed {
			t.Fatalf("a put naming %s: %d %s", name, c, b)
		}
	}
	if c, _, b := do(t, http.MethodPut, file, []byte("b"), map[string]string{"Authorization": "Bearer ok", "If-Match": tag}); c != http.StatusCreated {
		t.Fatalf("a put naming the version: %d %s", c, b)
	}
	s.Put("o", "files/m/d/c.md", Object{Data: []byte("c")})
	s.Put("o", "files/mx/e.md", Object{Data: []byte("e")})
	var page struct {
		Entries []struct {
			Path, Checksum string
		}
		Prefixes   []string
		NextCursor string `json:"next_cursor"`
	}
	c, _, b = do(t, http.MethodGet, s.URL()+"/files/o/files/m?list=1&limit=1", nil, bearer)
	if c != http.StatusOK || json.Unmarshal(b, &page) != nil || len(page.Entries) != 1 || page.Entries[0].Path != "files/m/a.md" || page.NextCursor != "files/m/a.md" {
		t.Fatalf("the first page: %d %s", c, b)
	}
	c, _, b = do(t, http.MethodGet, s.URL()+"/files/o/files/m?list=1&cursor="+page.NextCursor, nil, bearer)
	page.NextCursor = ""
	if c != http.StatusOK || json.Unmarshal(b, &page) != nil || len(page.Entries) != 1 || page.Entries[0].Path != "files/m/d/c.md" || page.Entries[0].Checksum != checksum([]byte("c")) ||
		len(page.Prefixes) != 1 || page.Prefixes[0] != "files/m/d/" || page.NextCursor != "" {
		t.Fatalf("the last page: %d %s", c, b)
	}
	if c, _, _ := do(t, http.MethodGet, s.URL()+"/files/o/files/m?list=1&limit=0", nil, bearer); c != http.StatusBadRequest {
		t.Fatalf("a limit of 0: %d", c)
	}
	if c, _, b := do(t, http.MethodDelete, file, nil, map[string]string{"Authorization": "Bearer ok", "If-Match": tag}); c != http.StatusPreconditionFailed {
		t.Fatalf("a delete naming an old version: %d %s", c, b)
	}
	if c, _, b := do(t, http.MethodDelete, file, nil, map[string]string{"Authorization": "Bearer ok", "If-Match": `"` + checksum([]byte("b")) + `"`}); c != http.StatusNoContent {
		t.Fatalf("a delete to the trash: %d %s", c, b)
	}
	if o, ok := s.Trashed("o", "files/m/a.md"); !ok || string(o.Data) != "b" {
		t.Fatalf("the trash holds %+v %t", o, ok)
	}
	s.Fail(OpList, Failure{Status: http.StatusForbidden, Code: "forbidden", Message: "No.", Detail: "memory_off: Off."})
	c, _, b = do(t, http.MethodGet, s.URL()+"/files/o/files/m?list=1", nil, bearer)
	if c != http.StatusForbidden || !strings.Contains(string(b), `"message":"No."`) || !strings.Contains(string(b), `"detail":"memory_off: Off."`) {
		t.Fatalf("an injected failure: %d %s", c, b)
	}
}
