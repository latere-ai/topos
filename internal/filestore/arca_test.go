// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package filestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/arcastub"
)

const owner = "https://issuer.example|ada"

func digest(b []byte) session.Digest {
	sum := sha256.Sum256(b)
	return session.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

func folder(s *arcastub.Server, path string) session.Folder {
	return session.Folder{URL: s.URL(), Owner: owner, Path: path}
}

func open(t *testing.T, f session.Folder, tok string) *Arca {
	t.Helper()
	a, err := New(f, func(context.Context) (string, error) { return tok, nil }, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func read(t *testing.T, a *Arca, ref string) ([]byte, int64) {
	t.Helper()
	rc, n, err := a.Open(t.Context(), ref)
	if err != nil {
		t.Fatalf("Open %s: %v", ref, err)
	}
	defer func() {
		if err := rc.Close(); err != nil {
			t.Error(err)
		}
	}()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b, n
}

// TestArcaPutsReadsAndCopies: a put at or below the bound is one
// create-only PUT with its length, its type and the bearer; a put onto a
// taken path is ErrFileExists; a larger put is an upload session whose
// parts reach the bucket with no bearer; a read follows the redirect to
// the bucket without the bearer and answers the length; a folder is
// copied on the server; and nothing is ErrFileNotFound (spec 068).
func TestArcaPutsReadsAndCopies(t *testing.T) {
	s := arcastub.New(t)
	s.RequireToken(func(tok string) bool { return tok == "session-token" })
	ctx := t.Context()
	a := open(t, folder(s, "files/sessions/ses_a"), "session-token")

	small := []byte("%PDF-1.7 report")
	d, err := a.Put(ctx, "results/1/out/report.pdf", "application/pdf", int64(len(small)), bytes.NewReader(small))
	if err != nil || d != digest(small) {
		t.Fatalf("Put: %s, %v", d, err)
	}
	o, ok := s.Object(owner, "files/sessions/ses_a/results/1/out/report.pdf")
	if !ok || !bytes.Equal(o.Data, small) || o.ContentType != "application/pdf" || o.Token != "session-token" {
		t.Fatalf("the object %+v %t", o, ok)
	}
	put := s.Requests()[0]
	if put.Header.Get("If-None-Match") != "*" || put.Header.Get("Content-Length") != "15" || put.Header.Get("Authorization") != "Bearer session-token" {
		t.Fatalf("the put's headers %v", put.Header)
	}
	if _, err := a.Put(ctx, "results/1/out/report.pdf", "application/pdf", int64(len(small)), bytes.NewReader([]byte("another report!"))); !errors.Is(err, harness.ErrFileExists) {
		t.Fatalf("a put onto a taken path: %v", err)
	}
	if o, _ := s.Object(owner, "files/sessions/ses_a/results/1/out/report.pdf"); !bytes.Equal(o.Data, small) {
		t.Fatal("a create-only put replaced the file")
	}
	empty, err := a.Put(ctx, "attachments/evt_1/empty.txt", "", 0, bytes.NewReader(nil))
	if err != nil || empty != digest(nil) {
		t.Fatalf("an empty put: %s, %v", empty, err)
	}

	s.SetPartSize(5 << 20)
	large := bytes.Repeat([]byte("0123456789abcdef"), (InlineBytes+(3<<20))/16)
	d, err = a.Put(ctx, "results/2/data.bin", "application/octet-stream", int64(len(large)), bytes.NewReader(large))
	if err != nil || d != digest(large) {
		t.Fatalf("a put past the bound: %s, %v", d, err)
	}
	if o, ok := s.Object(owner, "files/sessions/ses_a/results/2/data.bin"); !ok || !bytes.Equal(o.Data, large) {
		t.Fatal("the uploaded object is not the file")
	}
	var parts, completes int
	for _, r := range s.Requests() {
		switch r.Op {
		case arcastub.OpPart:
			parts++
		case arcastub.OpComplete:
			completes++
			if r.Header.Get("If-None-Match") != "*" || r.Header.Get("Authorization") != "Bearer session-token" {
				t.Fatalf("the completion's headers %v", r.Header)
			}
		}
	}
	if parts != 4 || completes != 1 {
		t.Fatalf("%d parts and %d completions", parts, completes)
	}

	got, n := read(t, a, "results/1/out/report.pdf")
	if !bytes.Equal(got, small) || n != int64(len(small)) {
		t.Fatalf("a read through the redirect: %q, %d", got, n)
	}
	s.RedirectAbove(1 << 20)
	if got, n := read(t, a, "results/1/out/report.pdf"); !bytes.Equal(got, small) || n != int64(len(small)) {
		t.Fatalf("an inline read: %q, %d", got, n)
	}
	if _, _, err := a.Open(ctx, "results/9/none.txt"); !errors.Is(err, harness.ErrFileNotFound) {
		t.Fatalf("a read of nothing: %v", err)
	}

	fork := open(t, folder(s, "files/sessions/ses_b"), "session-token")
	if err := fork.CopyFolder(ctx, folder(s, "files/sessions/ses_a"), "results/1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := read(t, fork, "results/1/out/report.pdf"); !bytes.Equal(got, small) {
		t.Fatalf("the copied file %q", got)
	}
	var copied []string
	for _, r := range s.Requests() {
		if r.Op == arcastub.OpCopy {
			copied = append(copied, r.Path)
		}
	}
	if len(copied) != 1 || !strings.HasSuffix(copied[0], "%7Cada/files/sessions/ses_a/results/1") || strings.Contains(copied[0], "|") {
		t.Fatalf("the copy asked %v", copied)
	}
	if err := fork.CopyFolder(ctx, folder(s, "files/sessions/ses_a"), "results/7"); !errors.Is(err, harness.ErrFileNotFound) {
		t.Fatalf("a copy of nothing: %v", err)
	}
	other := folder(s, "files/sessions/ses_a")
	other.Owner = "https://issuer.example|bob"
	if err := fork.CopyFolder(ctx, other, "results/1"); err == nil {
		t.Fatal("a copy across spaces was asked")
	}
	for _, dir := range []string{"", "/results", "results/../x", "results//1"} {
		if err := fork.CopyFolder(ctx, folder(s, "files/sessions/ses_a"), dir); err == nil {
			t.Fatalf("a copy of %q was asked", dir)
		}
	}
}

// TestArcaRefusals: a refusal for the size is FileTooLarge and any other
// refusal is not; a source shorter or longer than its size is
// ErrFileSizeChanged; an upload whose part fails is aborted; a bad ref, a
// token that cannot be had and a folder that names no files API are
// refused before any request.
func TestArcaRefusals(t *testing.T) {
	s := arcastub.New(t)
	ctx := t.Context()
	a := open(t, folder(s, "files/sessions/ses_a"), "tok")
	data := []byte("data")
	s.Fail(arcastub.OpPut, arcastub.Failure{Status: http.StatusRequestEntityTooLarge, Code: "object_too_large"})
	if _, err := a.Put(ctx, "results/1/a", "", 4, bytes.NewReader(data)); !harness.FileTooLarge(err) {
		t.Fatalf("a refusal for the size: %v", err)
	}
	s.Fail(arcastub.OpPut, arcastub.Failure{Status: http.StatusRequestEntityTooLarge, Code: "quota_exceeded"})
	if _, err := a.Put(ctx, "results/1/a", "", 4, bytes.NewReader(data)); harness.FileTooLarge(err) || err == nil {
		t.Fatalf("a refusal for the space's quota: %v", err)
	}
	s.Fail(arcastub.OpPut, arcastub.Failure{Status: http.StatusForbidden, Code: "forbidden"})
	_, err := a.Put(ctx, "results/1/a", "", 4, bytes.NewReader(data))
	if fe, ok := errors.AsType[*harness.FileError](err); !ok || fe.Code != "forbidden" || fe.Status != http.StatusForbidden {
		t.Fatalf("a refused put: %v", err)
	}
	if _, err := a.Put(ctx, "results/1/short", "", 10, bytes.NewReader(data)); !errors.Is(err, harness.ErrFileSizeChanged) {
		t.Fatalf("a source shorter than its size: %v", err)
	}
	if _, err := a.Put(ctx, "results/1/long", "", 2, bytes.NewReader(data)); !errors.Is(err, harness.ErrFileSizeChanged) {
		t.Fatalf("a source longer than its size: %v", err)
	}
	s.SetPartSize(8 << 20)
	s.Fail(arcastub.OpPart, arcastub.Failure{Status: http.StatusServiceUnavailable, Code: "unavailable"})
	large := bytes.Repeat([]byte{7}, InlineBytes+1)
	if _, err := a.Put(ctx, "results/1/large", "", int64(len(large)), bytes.NewReader(large)); err == nil {
		t.Fatal("an upload whose part failed succeeded")
	}
	if s.Uploads() != 0 {
		t.Fatal("a failed upload was left open")
	}
	s.Fail(arcastub.OpUpload, arcastub.Failure{Status: http.StatusForbidden, Code: "forbidden"})
	if _, err := a.Put(ctx, "results/1/large", "", int64(len(large)), bytes.NewReader(large)); err == nil {
		t.Fatal("a refused upload succeeded")
	}
	s.Fail(arcastub.OpComplete, arcastub.Failure{Status: http.StatusPreconditionFailed, Code: "precondition_failed"})
	if _, err := a.Put(ctx, "results/1/large", "", int64(len(large)), bytes.NewReader(large)); !errors.Is(err, harness.ErrFileExists) || s.Uploads() != 0 {
		t.Fatalf("a completion onto a taken path: %v, %d open", err, s.Uploads())
	}
	s.Fail(arcastub.OpRead, arcastub.Failure{Status: http.StatusServiceUnavailable, Code: "storage_unavailable"})
	if _, _, err := a.Open(ctx, "results/1/a"); err == nil || errors.Is(err, harness.ErrFileNotFound) {
		t.Fatalf("a refused read: %v", err)
	}
	s.Fail(arcastub.OpCopy, arcastub.Failure{Status: http.StatusConflict, Code: "path_taken"})
	if err := a.CopyFolder(ctx, folder(s, "files/sessions/ses_z"), "results/1"); err == nil {
		t.Fatal("a refused copy succeeded")
	}
	for _, ref := range []string{"report.pdf", "results/1/../x"} {
		if _, err := a.Put(ctx, ref, "", 4, bytes.NewReader(data)); !errors.Is(err, session.ErrInvalid) {
			t.Fatalf("a put at %q: %v", ref, err)
		}
		if _, _, err := a.Open(ctx, ref); !errors.Is(err, session.ErrInvalid) {
			t.Fatalf("a read at %q: %v", ref, err)
		}
	}
	if _, err := a.Put(ctx, "results/1/neg", "", -1, bytes.NewReader(data)); err == nil {
		t.Fatal("a put of a negative size")
	}
	broken, err := New(folder(s, "files/sessions/ses_a"), func(context.Context) (string, error) { return "", errors.New("no token") }, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broken.Put(ctx, "results/1/a", "", 4, bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), "no token") {
		t.Fatalf("a put with no token: %v", err)
	}
	if _, _, err := broken.Open(ctx, "results/1/a"); err == nil {
		t.Fatal("a read with no token")
	}
	if err := broken.CopyFolder(ctx, folder(s, "files/sessions/ses_a"), "results/1"); err == nil {
		t.Fatal("a copy with no token")
	}
	for _, f := range []session.Folder{{URL: "ftp://x", Owner: owner, Path: "files/p"}, {URL: "https://x", Path: "files/p"}, {URL: "https://x", Owner: owner}} {
		if _, err := New(f, func(context.Context) (string, error) { return "", nil }, &http.Client{}); err == nil {
			t.Fatalf("New(%+v)", f)
		}
	}
	if _, err := New(folder(s, "files/p"), nil, &http.Client{}); err == nil {
		t.Fatal("New with no token")
	}
}

// TestArcaDistrustsWhatTheFilesAPIHandsOut: a redirect to an address
// that is not http or https, a redirect with no address and a bucket that
// refuses the read fail the read; an upload that answers no session,
// parts that do not cover the size, a part address that is not http or
// https and a part answered with no ETag fail the put, each upload
// aborted, and an abort refused is joined to the put's error.
func TestArcaDistrustsWhatTheFilesAPIHandsOut(t *testing.T) {
	var mode string
	var aborts int
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/files/"):
			switch mode {
			case "scheme":
				w.Header().Set("Location", "file:///etc/passwd")
				w.WriteHeader(http.StatusFound)
			case "nowhere":
				w.WriteHeader(http.StatusFound)
			default:
				w.Header().Set("Location", srv.URL+"/bucket")
				w.WriteHeader(http.StatusFound)
			}
		case r.URL.Path == "/bucket":
			w.WriteHeader(http.StatusForbidden)
		case r.URL.Path == "/v1/uploads":
			switch mode {
			case "garbled":
				_, _ = io.WriteString(w, "not json")
			case "short":
				_, _ = io.WriteString(w, `{"id":"u1","part_size":1048576,"part_urls":["`+srv.URL+`/part"]}`)
			case "badpart":
				_, _ = io.WriteString(w, `{"id":"u1","part_size":16777216,"part_urls":["ftp://x/1","ftp://x/2"]}`)
			default:
				_, _ = io.WriteString(w, `{"id":"u1","part_size":16777216,"part_urls":["`+srv.URL+`/part","`+srv.URL+`/part"]}`)
			}
		case r.URL.Path == "/part":
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete:
			aborts++
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	ctx := t.Context()
	a := open(t, session.Folder{URL: srv.URL + "/v1", Owner: owner, Path: "files/p"}, "tok")
	for _, m := range []string{"scheme", "nowhere", "refused"} {
		mode = m
		if _, _, err := a.Open(ctx, "results/1/a"); err == nil {
			t.Fatalf("%s: the read succeeded", m)
		}
	}
	large := bytes.Repeat([]byte{1}, InlineBytes+1)
	for _, m := range []string{"garbled", "short", "badpart", "noetag"} {
		mode = m
		before := aborts
		_, err := a.Put(ctx, "results/1/large", "", int64(len(large)), bytes.NewReader(large))
		if err == nil {
			t.Fatalf("%s: the put succeeded", m)
		}
		if m != "garbled" && (aborts != before+1 || !strings.Contains(err.Error(), "500")) {
			t.Fatalf("%s: %d aborts, %v", m, aborts-before, err)
		}
	}
}

// TestArcaOverwritesAndDeletes: an overwrite replaces the file at its
// ref with no If-None-Match, in one PUT or through an upload whose
// completion names none; a delete removes it for good, permanent=1, a
// delete of nothing is no error, and a refused delete is the files API's
// refusal (spec 073).
func TestArcaOverwritesAndDeletes(t *testing.T) {
	s := arcastub.New(t)
	ctx := t.Context()
	a := open(t, folder(s, "files/sessions/ses_a"), "session-token")
	first, second := []byte("first"), []byte("second version")
	if _, err := a.Overwrite(ctx, "work/src/app.js", "text/javascript", int64(len(first)), bytes.NewReader(first)); err != nil {
		t.Fatal(err)
	}
	d, err := a.Overwrite(ctx, "work/src/app.js", "text/javascript", int64(len(second)), bytes.NewReader(second))
	if err != nil || d != digest(second) {
		t.Fatalf("an overwrite: %s, %v", d, err)
	}
	if o, _ := s.Object(owner, "files/sessions/ses_a/work/src/app.js"); !bytes.Equal(o.Data, second) {
		t.Fatalf("the file after the overwrite %q", o.Data)
	}
	s.SetPartSize(8 << 20)
	large := bytes.Repeat([]byte{9}, InlineBytes+1)
	if _, err := a.Put(ctx, "work/data.bin", "", int64(len(large)), bytes.NewReader(large)); err != nil {
		t.Fatal(err)
	}
	large[0] = 1
	if _, err := a.Overwrite(ctx, "work/data.bin", "", int64(len(large)), bytes.NewReader(large)); err != nil {
		t.Fatalf("an overwrite past the bound: %v", err)
	}
	if o, _ := s.Object(owner, "files/sessions/ses_a/work/data.bin"); !bytes.Equal(o.Data, large) {
		t.Fatal("the uploaded overwrite is not the file")
	}
	// The put of data.bin is the first completion and names If-None-Match;
	// every other write here is an overwrite and names none.
	var completes int
	for _, r := range s.Requests() {
		switch {
		case r.Op == arcastub.OpComplete:
			completes++
			if want := map[int]string{1: "*", 2: ""}[completes]; r.Header.Get("If-None-Match") != want {
				t.Fatalf("completion %d named If-None-Match %q", completes, r.Header.Get("If-None-Match"))
			}
		case r.Op == arcastub.OpPut && r.Header.Get("If-None-Match") != "":
			t.Fatalf("an overwrite named If-None-Match: %s %v", r.Path, r.Header)
		}
	}
	if completes != 2 {
		t.Fatalf("%d completions", completes)
	}

	if err := a.Delete(ctx, "work/src/app.js"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Object(owner, "files/sessions/ses_a/work/src/app.js"); ok {
		t.Fatal("the deleted file is still there")
	}
	if err := a.Delete(ctx, "work/src/app.js"); err != nil {
		t.Fatalf("a delete of nothing: %v", err)
	}
	s.Fail(arcastub.OpDelete, arcastub.Failure{Status: http.StatusForbidden, Code: "forbidden"})
	if err := a.Delete(ctx, "work/data.bin"); err == nil {
		t.Fatal("a refused delete succeeded")
	}
	if err := a.Delete(ctx, "report.pdf"); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("a delete of a bad ref: %v", err)
	}
	broken, err := New(folder(s, "files/sessions/ses_a"), func(context.Context) (string, error) { return "", errors.New("no token") }, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	if err := broken.Delete(ctx, "work/data.bin"); err == nil {
		t.Fatal("a delete with no token")
	}
}

// TestPresign (spec 080): a presign reads the ref with ?inline=0 and the
// bearer and answers the Location without following it, an address that
// reads the bytes with no credential; nothing at the ref is
// ErrFileNotFound, a ref out of the folder's rules is refused, and an
// answer of the bytes or of an address that is not http or https is an
// error.
func TestPresign(t *testing.T) {
	s := arcastub.New(t)
	f := folder(s, "files/conversations/s1")
	a := open(t, f, "tok")
	s.Put(owner, "files/conversations/s1/attachments/e1/a.pdf", arcastub.Object{Data: []byte("%PDF-1.7 small"), ContentType: "application/pdf"})
	u, err := a.Presign(t.Context(), "attachments/e1/a.pdf")
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	for _, r := range s.Requests() {
		if r.Op == arcastub.OpRead {
			reads++
			if r.Header.Get("Authorization") != "Bearer tok" || r.Query != "inline=0" {
				t.Fatalf("the presign's read %+v", r)
			}
		}
	}
	if reads != 1 {
		t.Fatalf("%d reads of the files API", reads)
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if cerr := resp.Body.Close(); err != nil || cerr != nil || string(body) != "%PDF-1.7 small" {
		t.Fatalf("the presigned address %s answered %q %v %v", u, body, err, cerr)
	}
	if _, err := a.Presign(t.Context(), "attachments/e1/gone.pdf"); !errors.Is(err, harness.ErrFileNotFound) {
		t.Fatalf("nothing at the ref: %v", err)
	}
	if _, err := a.Presign(t.Context(), "../x"); err == nil {
		t.Fatal("a ref out of the folder was presigned")
	}

	var location string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if location == "" {
			_, _ = io.WriteString(w, "the bytes")
			return
		}
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	b, err := New(session.Folder{URL: srv.URL, Owner: owner, Path: "files/conversations/s1"}, func(context.Context) (string, error) { return "tok", nil }, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	for _, loc := range []string{"", "ftp://bucket.example/a"} {
		location = loc
		if _, err := b.Presign(t.Context(), "attachments/e1/a.pdf"); err == nil {
			t.Errorf("a presign answered with %q was taken", loc)
		}
	}
}
