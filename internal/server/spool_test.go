// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/internal/filestore"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/arcastub"
)

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

// patternDigest is the digest of n bytes of the pattern.
func patternDigest(t *testing.T, n int64) session.Digest {
	t.Helper()
	h := sha256.New()
	if _, err := io.Copy(h, &pattern{n: n}); err != nil {
		t.Fatal(err)
	}
	return session.Digest("sha256:" + hex.EncodeToString(h.Sum(nil)))
}

// spoolFiles are the files in a spool directory.
func spoolFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// TestALargeSourceIsStreamedIntoTheFolder (spec 081): a source past the
// files API's inline bound, which the files API redirects to its bucket,
// is read there with no bearer, written to the session's folder through
// an upload session with the session's own token, and recorded with its
// size, digest and the bucket's media type; nothing is left in the spool.
func TestALargeSourceIsStreamedIntoTheFolder(t *testing.T) {
	f := newSourceFixture(t)
	f.arca.SetPartSize(8 << 20)
	s := f.create("alice", "reviewer")
	data := make([]byte, 40<<20)
	if _, err := io.ReadFull(&pattern{n: int64(len(data))}, data); err != nil {
		t.Fatal(err)
	}
	f.arca.Put("me", "files/Scans/report.pdf", arcastub.Object{Data: data, ContentType: "application/pdf"})
	a, ev := f.sendFiles(s.ID, source("report.pdf", "me", "files/Scans/report.pdf"))
	if a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	at := session.AttachmentPath(ev.ID, "report.pdf")
	want := session.Attachment{Name: "report.pdf", MediaType: "application/pdf", Size: int64(len(data)), Blob: session.DigestOf(data), Path: at, Ref: at,
		Source: session.AttachmentSource{Owner: "me", Path: "files/Scans/report.pdf"}}
	if got := records(t, ev); len(got) != 1 || got[0] != want {
		t.Fatalf("the record %+v, want %+v", got, want)
	}
	if o, ok := f.arca.Object(space, folderPath(s.ID)+"/"+at); !ok || !bytes.Equal(o.Data, data) || o.Token != "session:"+s.ID {
		t.Fatalf("the folder holds %d bytes %t", len(o.Data), ok)
	}
	ops := map[string]int{}
	for _, r := range f.arca.Requests() {
		ops[r.Op]++
		if r.Op == arcastub.OpRead && r.Header.Get("Authorization") != "Bearer alice" {
			t.Fatalf("the files API's read carried %q", r.Header.Get("Authorization"))
		}
	}
	// The stub's bucket fails the test on a request with an Authorization
	// header, so the read there and each part carried none.
	if ops[arcastub.OpRead] != 1 || ops["bucket-read"] != 1 || ops[arcastub.OpUpload] != 1 || ops[arcastub.OpPart] != 5 || ops[arcastub.OpComplete] != 1 || ops[arcastub.OpPut] != 0 {
		t.Fatalf("the files API's requests %v", ops)
	}
	if left := spoolFiles(t, f.spool); len(left) != 0 || f.api.sources.used.Load() != 0 {
		t.Fatalf("the spool holds %v, %d bytes counted", left, f.api.sources.used.Load())
	}
}

// discard is a folder that hashes what it is written and keeps only the
// digest and the length, so a test measures what the server allocates
// and not what a stub keeps.
type discard struct {
	mu   sync.Mutex
	puts map[string]session.Digest
}

func (d *discard) Put(_ context.Context, ref, _ string, size int64, r io.Reader) (session.Digest, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	switch {
	case err != nil:
		return "", err
	case n != size:
		return "", fmt.Errorf("%s: read %d of %d bytes", ref, n, size)
	}
	dg := session.Digest("sha256:" + hex.EncodeToString(h.Sum(nil)))
	d.mu.Lock()
	defer d.mu.Unlock()
	d.puts[ref] = dg
	return dg, nil
}

func (d *discard) Overwrite(ctx context.Context, ref, media string, size int64, r io.Reader) (session.Digest, error) {
	return d.Put(ctx, ref, media, size, r)
}

func (d *discard) Delete(context.Context, string) error { return nil }

func (d *discard) Open(context.Context, string) (io.ReadCloser, int64, error) {
	return nil, 0, harness.ErrFileNotFound
}

func (d *discard) CopyFolder(context.Context, session.Folder, string) error { return nil }

// generated is a stub files API on https that redirects every read to its
// own bucket, which answers n bytes of the pattern with their length.
func generated(t *testing.T, n int64) *httptest.Server {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bucket" {
			http.Redirect(w, r, "/bucket", http.StatusFound)
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("the bucket got an Authorization header")
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
		// A write to a closed connection is the client's to notice.
		_, _ = io.Copy(w, &pattern{n: n})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestALargeSourceIsNotHeldInMemory (spec 081): a send whose source is
// four times the old 16 MiB bound allocates a small part of the file:
// the source is spooled to disk and written to the folder as a stream.
func TestALargeSourceIsNotHeldInMemory(t *testing.T) {
	const size = 64 << 20
	f := newSourceFixture(t)
	folder := &discard{puts: map[string]session.Digest{}}
	files := generated(t, size)
	f.api.o.Files.Write = func(context.Context, session.Session) (harness.Files, error) { return folder, nil }
	f.api.o.Files.Source = func(ctx context.Context, _, token string, src session.AttachmentSource, max int64) (*filestore.Source, error) {
		return filestore.ReadSource(ctx, files.Client(), files.URL, token, src, max)
	}
	s := f.create("alice", "reviewer")
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	a, ev := f.sendFiles(s.ID, source("export.zip", "me", "files/export.zip"))
	runtime.ReadMemStats(&after)
	if a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > size/16 {
		t.Fatalf("a send of a %d byte source allocated %d bytes", size, alloc)
	}
	want := patternDigest(t, size)
	got := records(t, ev)
	if len(got) != 1 || got[0].Size != size || got[0].Blob != want || got[0].MediaType != "application/zip" || folder.puts[got[0].Ref] != want {
		t.Fatalf("the record %+v, the folder's digest %s", got, folder.puts)
	}
}

// TestSpoolFilesAreRemoved (spec 081): a source past a short text is
// spooled while its send is checked, and its spool is removed once the
// message is taken, refused or kept in a fork; a server's start removes
// the spool files a stopped one left, and leaves the rest of the
// directory alone.
func TestSpoolFilesAreRemoved(t *testing.T) {
	f := newSourceFixture(t)
	parent := f.create("alice", "reviewer")
	f.turn(parent.ID, 1, "Hello.", 1)
	big := bytes.Repeat([]byte("0123456789abcdef"), session.InlineTextBytes/16+1)
	f.arca.Put("me", "files/big.bin", arcastub.Object{Data: big, ContentType: "application/octet-stream"})
	// seen is the most spool files the spool held while a source was
	// read, counted as the read closes its source.
	var mu sync.Mutex
	seen := 0
	read := f.api.o.Files.Source
	f.api.o.Files.Source = func(ctx context.Context, root, token string, src session.AttachmentSource, bound int64) (*filestore.Source, error) {
		s, err := read(ctx, root, token, src, bound)
		if err != nil {
			return nil, err
		}
		s.Body = closer{ReadCloser: s.Body, close: func() {
			mu.Lock()
			defer mu.Unlock()
			seen = max(seen, len(spoolFiles(t, f.spool)))
		}}
		return s, nil
	}
	empty := func(what string) {
		t.Helper()
		mu.Lock()
		held := seen
		seen = 0
		mu.Unlock()
		if held != 1 {
			t.Fatalf("%s: the spool held %d files during the read", what, held)
		}
		if left := spoolFiles(t, f.spool); len(left) != 0 || f.api.sources.used.Load() != 0 {
			t.Fatalf("%s left %v in the spool, %d bytes counted", what, left, f.api.sources.used.Load())
		}
	}
	a, sent := f.sendFiles(parent.ID, source("big.bin", "me", "files/big.bin"))
	if a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	if r := records(t, sent); len(r) != 1 || r[0].Size != int64(len(big)) || r[0].Blob != session.DigestOf(big) || r[0].Inline {
		t.Fatalf("the record %+v", r)
	}
	empty("a taken message")
	if a, _ := f.sendFiles(parent.ID, source("big.bin", "me", "files/big.bin"), source("gone.bin", "me", "files/gone.bin")); a.status != http.StatusNotFound {
		t.Fatalf("a message whose second source is gone: %d %s", a.status, a.body)
	}
	empty("a refused message")
	f.turn(parent.ID, 2, "Read it.", 1)
	f.forked(parent.ID, `{"message":{"content":[{"type":"text","text":"Again."}],"attachments":[`+source("big.bin", "me", "files/big.bin")+`]}}`)
	empty("a fork")

	dir := filepath.Join(t.TempDir(), "spool")
	if _, err := newSources(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{spoolPrefix + "1234", "kept.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := newSources(dir); err != nil {
		t.Fatal(err)
	}
	if left := spoolFiles(t, dir); len(left) != 1 || left[0] != "kept.txt" {
		t.Fatalf("a start left %v", left)
	}
}

// closer is a ReadCloser that calls close before it closes.
type closer struct {
	io.ReadCloser
	close func()
}

func (c closer) Close() error {
	c.close()
	return c.ReadCloser.Close()
}

// TestTheSpoolIsBound (spec 081): the spools of the messages a server
// reads at once are held to their bound together; a source that would
// pass it is files_unavailable, which may be sent again, and nothing is
// appended, written or left counted. The bound is lowered, so the test
// spools kilobytes and not gigabytes.
func TestTheSpoolIsBound(t *testing.T) {
	f := newSourceFixture(t)
	s := f.create("alice", "reviewer")
	big := bytes.Repeat([]byte{'z'}, 4*session.InlineTextBytes)
	f.arca.Put("me", "files/big.bin", arcastub.Object{Data: big})
	f.api.sources.spool = 3 * session.InlineTextBytes
	last := f.stored(s.ID).LastSeq
	a, _ := f.sendFiles(s.ID, source("big.bin", "me", "files/big.bin"))
	if a.status != http.StatusServiceUnavailable || a.code() != CodeFilesUnavailable || !bytes.Contains(a.body, []byte("sent again")) {
		t.Fatalf("a source past the spool's room: %d %s", a.status, a.body)
	}
	if got := f.stored(s.ID).LastSeq; got != last {
		t.Fatalf("the refused message appended to seq %d", got)
	}
	if p := f.arca.Paths(space, folderPath(s.ID)+"/"); len(p) != 0 {
		t.Fatalf("the refused message wrote %v", p)
	}
	if left := spoolFiles(t, f.spool); len(left) != 0 || f.api.sources.used.Load() != 0 {
		t.Fatalf("the spool holds %v, %d bytes counted", left, f.api.sources.used.Load())
	}
	f.api.sources.spool = MaxSpoolBytes
	if a, _ := f.sendFiles(s.ID, source("big.bin", "me", "files/big.bin")); a.status != http.StatusOK {
		t.Fatalf("the message sent again: %d %s", a.status, a.body)
	}
}

// TestASpoolFailureIsReported: a spool that cannot be made, as in a
// spool directory that is gone, refuses the message as files_unavailable
// and counts nothing (spec 081).
func TestASpoolFailureIsReported(t *testing.T) {
	f := newSourceFixture(t)
	s := f.create("alice", "reviewer")
	f.arca.Put("me", "files/big.bin", arcastub.Object{Data: bytes.Repeat([]byte{'z'}, 2*session.InlineTextBytes)})
	if err := os.RemoveAll(f.spool); err != nil {
		t.Fatal(err)
	}
	a, _ := f.sendFiles(s.ID, source("big.bin", "me", "files/big.bin"))
	if a.status != http.StatusServiceUnavailable || a.code() != CodeFilesUnavailable || f.api.sources.used.Load() != 0 {
		t.Fatalf("a spool that cannot be made: %d %s", a.status, a.body)
	}
	if _, err := os.Stat(f.spool); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the spool directory: %v", err)
	}
}
