// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package filestore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/arcastub"
)

// readAll reads a source whole, as the server spools it, and closes it.
func readAll(ctx context.Context, client *http.Client, root, token string, src session.AttachmentSource, max int64) ([]byte, string, error) {
	s, err := ReadSource(ctx, client, root, token, src, max)
	if err != nil {
		return nil, "", err
	}
	data, err := io.ReadAll(s.Body)
	if err = errors.Join(err, s.Body.Close()); err != nil {
		return nil, "", err
	}
	if s.Size >= 0 && int64(len(data)) != s.Size {
		return nil, "", fmt.Errorf("the answer declared %d bytes and carried %d", s.Size, len(data))
	}
	return data, s.MediaType, nil
}

// TestReadSource: a source is read at its owner's path with the bearer
// given, at ?version=N when it names one, with the files API's
// Content-Type; a 401, a 403 and a 404 are ErrSourceNotFound alike, a body
// past the bound ErrSourceTooLarge, a redirect to an address that is not
// https is never followed, and any other refusal is the files API's (spec
// 079, 081).
func TestReadSource(t *testing.T) {
	s := arcastub.New(t)
	s.RedirectAbove(1 << 20)
	s.Put("me", "files/Reports/q 3.csv", arcastub.Object{Data: []byte("a,b\n"), ContentType: "text/csv"})
	s.PutVersion("me", "files/Reports/q 3.csv", 2, arcastub.Object{Data: []byte("old\n"), ContentType: "text/csv"})
	ctx := t.Context()
	src := session.AttachmentSource{Owner: "me", Path: "files/Reports/q 3.csv"}
	data, media, err := readAll(ctx, &http.Client{}, s.URL()+"/", "alice", src, 16)
	if err != nil || string(data) != "a,b\n" || media != "text/csv" {
		t.Fatalf("a read: %q %q %v", data, media, err)
	}
	r := s.Requests()[0]
	if r.Op != arcastub.OpRead || r.Path != "/v1/files/me/files/Reports/q%203.csv" || r.Query != "" || r.Header.Get("Authorization") != "Bearer alice" {
		t.Fatalf("the read is %+v", r)
	}
	src.Version = 2
	if data, _, err := readAll(ctx, &http.Client{}, s.URL(), "alice", src, 16); err != nil || string(data) != "old\n" || s.Requests()[1].Query != "version=2" {
		t.Fatalf("a version: %q %v %+v", data, err, s.Requests()[1])
	}
	src.Version = 0

	if _, _, err := readAll(ctx, &http.Client{}, s.URL(), "alice", src, 3); !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("a file past the bound: %v", err)
	}
	// The stub's bucket is on http, which a source's read never follows.
	s.RedirectAbove(-1)
	if _, _, err := readAll(ctx, &http.Client{}, s.URL(), "alice", src, 16); err == nil || errors.Is(err, ErrSourceTooLarge) || errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("a redirect to http: %v", err)
	}
	for _, r := range s.Requests() {
		if r.Op == "bucket-read" {
			t.Fatal("a redirect to http was followed to the bucket")
		}
	}
	s.RedirectAbove(1 << 20)

	missing := session.AttachmentSource{Owner: "me", Path: "files/none.csv"}
	if _, _, err := readAll(ctx, &http.Client{}, s.URL(), "alice", missing, 16); !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("a missing file: %v", err)
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		s.Fail(arcastub.OpRead, arcastub.Failure{Status: status, Code: "refused"})
		if _, _, err := readAll(ctx, &http.Client{}, s.URL(), "alice", src, 16); !errors.Is(err, ErrSourceNotFound) {
			t.Fatalf("a %d: %v", status, err)
		}
	}
	s.Fail(arcastub.OpRead, arcastub.Failure{Status: http.StatusInternalServerError, Code: "internal"})
	_, _, err = readAll(ctx, &http.Client{}, s.URL(), "alice", src, 16)
	if fe, ok := errors.AsType[*harness.FileError](err); !ok || fe.Status != http.StatusInternalServerError || fe.Code != "internal" {
		t.Fatalf("a failure of the files API: %v", err)
	}

	for _, c := range []struct {
		root, token string
		client      *http.Client
	}{{"ftp://files.example", "alice", &http.Client{}}, {"", "alice", &http.Client{}}, {s.URL(), "", &http.Client{}}, {s.URL(), "alice", nil}} {
		if _, _, err := readAll(ctx, c.client, c.root, c.token, src, 16); err == nil || errors.Is(err, ErrSourceNotFound) {
			t.Fatalf("a read at %q with %q: %v", c.root, c.token, err)
		}
	}
	// A body that names no length is held to the bound as it is read.
	chunked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for range 4 {
			if _, err := w.Write([]byte("12345")); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}))
	defer chunked.Close()
	if _, _, err := readAll(ctx, &http.Client{}, chunked.URL, "alice", src, 16); !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("a body past the bound with no length: %v", err)
	}
	if data, media, err := readAll(ctx, &http.Client{}, chunked.URL, "alice", src, 20); err != nil || len(data) != 20 || media == "" {
		t.Fatalf("a body at the bound with no length: %d %q %v", len(data), media, err)
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	if _, _, err := readAll(ctx, &http.Client{}, gone.URL, "alice", src, 16); err == nil || errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("a files API that does not answer: %v", err)
	}
}

// pattern is n bytes of a fixed pattern, generated as they are read, so a
// test serves and checks a large file without holding it.
type pattern struct{ off, n int64 }

func (p *pattern) Read(b []byte) (int, error) {
	if p.off >= p.n {
		return 0, io.EOF
	}
	b = b[:min(int64(len(b)), p.n-p.off)]
	for i := range b {
		b[i] = byte((p.off + int64(i)) % 251)
	}
	p.off += int64(len(b))
	return len(b), nil
}

// objectStore is a stub of an object store that presigned addresses name:
// on https, it answers /obj?n=N with N bytes of the pattern and its
// length, /chunked?n=N with them and no length, /again with a redirect to
// /obj, and /denied with 403, and records every request.
type objectStore struct {
	*httptest.Server
	mu    sync.Mutex
	seen  []url.URL
	types string
}

func newObjectStore(t *testing.T) *objectStore {
	o := &objectStore{types: "application/pdf"}
	o.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		o.seen = append(o.seen, *r.URL)
		o.mu.Unlock()
		if r.Header.Get("Authorization") != "" {
			t.Errorf("the object store got an Authorization header on %s", r.URL)
		}
		n, _ := strconv.ParseInt(r.URL.Query().Get("n"), 10, 64)
		switch r.URL.Path {
		case "/obj":
			w.Header().Set("Content-Type", o.types)
			w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
		case "/chunked":
			// A flush before the body sends it chunked, with no length.
			w.Header().Set("Content-Type", o.types)
			w.(http.Flusher).Flush()
		case "/again":
			http.Redirect(w, r, "/obj?n=4", http.StatusFound)
			return
		default:
			w.WriteHeader(http.StatusForbidden)
			return
		}
		// A write to a closed connection is the client's to notice.
		_, _ = io.Copy(w, &pattern{n: n})
	}))
	t.Cleanup(o.Close)
	return o
}

func (o *objectStore) requests() []url.URL {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]url.URL(nil), o.seen...)
}

// storage is a stub files API that answers every read with a redirect to
// location, and records the bearer each read carried.
func storage(t *testing.T, location *string) (*httptest.Server, *[]string) {
	var mu sync.Mutex
	var bearers []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		bearers = append(bearers, r.Header.Get("Authorization"))
		mu.Unlock()
		if *location != "" {
			w.Header().Set("Location", *location)
		}
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(s.Close)
	return s, &bearers
}

// TestReadSourceFollowsOneRedirect (spec 081): a read the files API
// redirects is followed once, to the https address it names, with no
// Authorization header, and answers the object store's bytes, length and
// Content-Type; a second redirect, an address on http, a redirect that
// names none and a refusal of the object store are failures of the files
// service, and a length or a body past the bound is ErrSourceTooLarge.
func TestReadSourceFollowsOneRedirect(t *testing.T) {
	ctx := t.Context()
	objects := newObjectStore(t)
	var location string
	files, bearers := storage(t, &location)
	src := session.AttachmentSource{Owner: "me", Path: "files/Reports/scan.pdf"}

	location = objects.URL + "/obj?n=1000"
	data, media, err := readAll(ctx, objects.Client(), files.URL, "alice", src, 1000)
	want, _ := io.ReadAll(&pattern{n: 1000})
	if err != nil || string(data) != string(want) || media != "application/pdf" {
		t.Fatalf("a redirected read: %d bytes, %q, %v", len(data), media, err)
	}
	if got := *bearers; len(got) != 1 || got[0] != "Bearer alice" {
		t.Fatalf("the files API got %q", got)
	}
	if r := objects.requests(); len(r) != 1 || r[0].RawQuery != "n=1000" {
		t.Fatalf("the object store got %d requests", len(r))
	}

	refused := func(what string, check func(error) bool) {
		t.Helper()
		if _, _, err := readAll(ctx, objects.Client(), files.URL, "alice", src, 1000); !check(err) {
			t.Fatalf("%s: %v", what, err)
		}
	}
	unavailable := func(err error) bool {
		return err != nil && !errors.Is(err, ErrSourceTooLarge) && !errors.Is(err, ErrSourceNotFound)
	}
	location = objects.URL + "/again"
	refused("a second redirect", unavailable)
	if r := objects.requests(); r[len(r)-1].Path != "/again" {
		t.Fatalf("the second redirect was followed to %s", r[len(r)-1].String())
	}
	before := len(objects.requests())
	location = "http://" + objects.Listener.Addr().String() + "/obj?n=4"
	refused("an address on http", unavailable)
	location = ""
	refused("a redirect that names no address", unavailable)
	if len(objects.requests()) != before {
		t.Fatal("a redirect that is not to https was followed")
	}
	location = objects.URL + "/denied"
	refused("a refusal of the object store", unavailable)

	location = objects.URL + "/obj?n=1001"
	refused("a length past the bound", func(err error) bool { return errors.Is(err, ErrSourceTooLarge) })
	location = objects.URL + "/chunked?n=1001"
	s, err := ReadSource(ctx, objects.Client(), files.URL, "alice", src, 1000)
	if err != nil || s.Size != -1 {
		t.Fatalf("a body with no length: %+v %v", s, err)
	}
	_, err = io.ReadAll(s.Body)
	if cerr := s.Body.Close(); !errors.Is(err, ErrSourceTooLarge) || cerr != nil {
		t.Fatalf("a body past the bound with no length: %v %v", err, cerr)
	}
	location = objects.URL + "/chunked?n=1000"
	if data, _, err := readAll(ctx, objects.Client(), files.URL, "alice", src, 1000); err != nil || len(data) != 1000 {
		t.Fatalf("a body at the bound with no length: %d %v", len(data), err)
	}
}

// TestReadSourceStreams (spec 081): a source past the old 16 MiB bound is
// read as a stream, whose reading allocates a part of the file: a read
// that holds it whole allocates at least its size. The bound is half the
// size, since the race detector's buffer pools drop buffers, and a run
// under it allocates about a sixth of the file where one without
// allocates under a hundredth.
func TestReadSourceStreams(t *testing.T) {
	const size = 64 << 20
	objects := newObjectStore(t)
	location := objects.URL + "/obj?n=" + strconv.Itoa(size)
	files, _ := storage(t, &location)
	src := session.AttachmentSource{Owner: "me", Path: "files/large.bin"}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	s, err := ReadSource(t.Context(), objects.Client(), files.URL, "alice", src, size)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	n, err := io.Copy(h, s.Body)
	if err = errors.Join(err, s.Body.Close()); err != nil || n != size || s.Size != size {
		t.Fatalf("read %d of %d bytes: %v", n, s.Size, err)
	}
	runtime.ReadMemStats(&after)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > size/2 {
		t.Fatalf("a read of %d bytes allocated %d", size, alloc)
	}
	w := sha256.New()
	if _, err := io.Copy(w, &pattern{n: size}); err != nil || string(w.Sum(nil)) != string(h.Sum(nil)) {
		t.Fatalf("the bytes read are not the file's: %v", err)
	}
}

// TestAPresignedAddressIsNamedByItsHostAlone: a read whose presigned
// address cannot be reached fails naming the address by its scheme and
// host, never its query, the credential that reads the file, nor its path,
// which names the person's file; the error reaches the log and the
// sender's answer (spec 081).
func TestAPresignedAddressIsNamedByItsHostAlone(t *testing.T) {
	closed := httptest.NewUnstartedServer(http.NotFoundHandler())
	host := closed.Listener.Addr().String()
	if err := closed.Listener.Close(); err != nil {
		t.Fatal(err)
	}
	signed := "https://" + host + "/latere-storage/u-7f3/obj-91?X-Amz-Credential=key&X-Amz-Signature=5ec7e7"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, signed, http.StatusFound)
	}))
	t.Cleanup(api.Close)
	leaks := func(err error) bool {
		for _, s := range []string{"X-Amz", "5ec7e7", "obj-91", "latere-storage"} {
			if strings.Contains(err.Error(), s) {
				return true
			}
		}
		return false
	}

	src := session.AttachmentSource{Owner: "me", Path: "files/Taxes/return-2026.pdf"}
	_, err := ReadSource(t.Context(), &http.Client{}, api.URL+"/", "alice", src, 1<<20)
	if err == nil || leaks(err) || !strings.Contains(err.Error(), "https://"+host) {
		t.Fatalf("a source at an address that cannot be reached: %v", err)
	}
	a, err := New(session.Folder{URL: api.URL, Owner: owner, Path: "files/conversations/s1"}, func(context.Context) (string, error) { return "tok", nil }, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Open(t.Context(), "attachments/e1/a.pdf"); err == nil || leaks(err) || !strings.Contains(err.Error(), "https://"+host) {
		t.Fatalf("a file of the folder at an address that cannot be reached: %v", err)
	}
	signed = "http://" + host + "/latere-storage/u-7f3/obj-91?X-Amz-Signature=5ec7e7"
	if _, err := ReadSource(t.Context(), &http.Client{}, api.URL+"/", "alice", src, 1<<20); err == nil || leaks(err) {
		t.Fatalf("a source redirected to an http address: %v", err)
	}
}
