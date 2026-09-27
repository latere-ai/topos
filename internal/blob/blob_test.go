// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package blob

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"latere.ai/x/pkg/s3/s3test"

	"latere.ai/x/topos/session"
)

// TestBlobStoreLocations: a body put into the file:// and the s3://
// store reads back by its session and digest at the path and the key
// the spec names; a body not there is ErrNotFound; one body and one
// session's bodies delete; Sessions names the sessions holding bodies.
func TestBlobStoreLocations(t *testing.T) {
	root := t.TempDir()
	objects := s3test.New(t, "bucket")
	stores := map[string]session.Blobs{
		"file": NewDir(root),
		"s3":   NewS3(objects.Client(true), "/topos/blobs/"),
	}
	a, b := session.NewID(session.PrefixSession), session.NewID(session.PrefixSession)
	one, two := []byte("a raw response body"), []byte("a captured request")
	d1, d2 := session.DigestOf(one), session.DigestOf(two)
	for name, st := range stores {
		ctx := t.Context()
		for _, put := range []struct {
			id   string
			d    session.Digest
			body []byte
		}{{a, d1, one}, {a, d2, two}, {b, d1, one}, {a, d1, one}} {
			if err := st.PutBlob(ctx, put.id, put.d, put.body); err != nil {
				t.Fatalf("%s: put: %v", name, err)
			}
		}
		if got, err := st.GetBlob(ctx, a, d2); err != nil || string(got) != string(two) {
			t.Fatalf("%s: get %q, %v", name, got, err)
		}
		if _, err := st.GetBlob(ctx, b, d2); !errors.Is(err, session.ErrNotFound) {
			t.Fatalf("%s: a body not there: %v", name, err)
		}
		ids, err := st.Sessions(ctx)
		slices.Sort(ids)
		want := []string{a, b}
		slices.Sort(want)
		if err != nil || !slices.Equal(ids, want) {
			t.Fatalf("%s: sessions %v, %v", name, ids, err)
		}
		if err := st.DeleteBlob(ctx, a, d2); err != nil {
			t.Fatal(err)
		}
		if _, err := st.GetBlob(ctx, a, d2); !errors.Is(err, session.ErrNotFound) {
			t.Fatalf("%s: a deleted body: %v", name, err)
		}
		if err := st.DeleteSession(ctx, a); err != nil {
			t.Fatal(err)
		}
		if _, err := st.GetBlob(ctx, a, d1); !errors.Is(err, session.ErrNotFound) {
			t.Fatalf("%s: a deleted session's body: %v", name, err)
		}
		if got, err := st.GetBlob(ctx, b, d1); err != nil || string(got) != string(one) {
			t.Fatalf("%s: another session's body %q, %v", name, got, err)
		}
		if err := st.PutBlob(ctx, "not-a-session", d1, one); !errors.Is(err, session.ErrBadID) {
			t.Fatalf("%s: a bad session id: %v", name, err)
		}
		if err := st.PutBlob(ctx, a, "sha256:nope", one); !errors.Is(err, session.ErrInvalid) {
			t.Fatalf("%s: a bad digest: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, b, d1.Hex())); err != nil {
		t.Fatalf("the file store's path: %v", err)
	}
	if _, ok := objects.Get("topos/blobs/" + b + "/" + d1.Hex()); !ok {
		t.Fatalf("the object store's key; it holds %v", objects.Keys())
	}
}

// TestOpenReadsTheURL: an empty URL keeps blobs in the store, file://
// and s3:// open their stores, and any other form is refused.
func TestOpenReadsTheURL(t *testing.T) {
	if st, err := Open("", "", "", nil); err != nil || st != nil {
		t.Fatalf("an empty URL: %v, %v", st, err)
	}
	if st, err := Open("file:///var/lib/topos/blobs", "", "", nil); err != nil {
		t.Fatal(err)
	} else if d, ok := st.(*Dir); !ok || d.root != "/var/lib/topos/blobs" {
		t.Fatalf("file:// opened %#v", st)
	}
	st, err := Open("s3://objects.example/bucket/topos/blobs?region=eu-central-1", "key", "secret", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if o, ok := st.(*S3); !ok || o.prefix != "topos/blobs" {
		t.Fatalf("s3:// opened %#v", st)
	}
	for _, bad := range []string{"relative/path", "file://host/path", "file:relative", "s3://host", "s3:///bucket", "https://objects.example/bucket", "%"} {
		if _, err := Open(bad, "key", "secret", nil); err == nil {
			t.Errorf("%q opened", bad)
		}
	}
	if _, err := Open("s3://objects.example/bucket", "", "", nil); err == nil {
		t.Error("an s3:// store without its keys opened")
	}
}

// TestS3ListsEveryPageAndReportsItsFailures: a session's bodies and the
// sessions holding bodies are read across listing pages, and a refused
// request fails each call with the key it named.
func TestS3ListsEveryPageAndReportsItsFailures(t *testing.T) {
	ctx := t.Context()
	objects := s3test.New(t, "bucket")
	st := NewS3(objects.Client(true), "")
	st.page = 1
	ids := []string{session.NewID(session.PrefixSession), session.NewID(session.PrefixSession), session.NewID(session.PrefixSession)}
	for _, id := range ids {
		for _, body := range []string{"one", "two", "three"} {
			if err := st.PutBlob(ctx, id, session.DigestOf([]byte(body)), []byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := st.Sessions(ctx)
	slices.Sort(got)
	if err != nil || !slices.Equal(got, ids) {
		t.Fatalf("sessions across pages %v, %v", got, err)
	}
	if err := st.DeleteSession(ctx, ids[0]); err != nil {
		t.Fatal(err)
	}
	if n := len(objects.Keys()); n != 6 {
		t.Fatalf("%d objects after one session's delete, want 6: %v", n, objects.Keys())
	}
	d := session.DigestOf([]byte("one"))
	for name, call := range map[string]func() error{
		"put":            func() error { return st.PutBlob(ctx, ids[1], d, []byte("one")) },
		"get":            func() error { _, err := st.GetBlob(ctx, ids[1], d); return err },
		"delete":         func() error { return st.DeleteBlob(ctx, ids[1], d) },
		"delete session": func() error { return st.DeleteSession(ctx, ids[1]) },
		"sessions":       func() error { _, err := st.Sessions(ctx); return err },
	} {
		objects.Fail(1, http.StatusForbidden)
		if err := call(); err == nil {
			t.Errorf("%s: a refused request reported nothing", name)
		}
	}
	for name, call := range map[string]func() error{
		"get":            func() error { _, err := st.GetBlob(ctx, "bad", d); return err },
		"delete":         func() error { return st.DeleteBlob(ctx, ids[1], "sha256:nope") },
		"delete session": func() error { return st.DeleteSession(ctx, "bad") },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: a bad id or digest was sent", name)
		}
	}
}

// TestDirReportsWhatTheFilesystemRefuses: a root under a file refuses
// every write and listing, a missing root lists no session, and a body
// path that is a directory cannot be read.
func TestDirReportsWhatTheFilesystemRefuses(t *testing.T) {
	ctx := t.Context()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	id, body := session.NewID(session.PrefixSession), []byte("body")
	d := session.DigestOf(body)
	blocked := NewDir(filepath.Join(file, "blobs"))
	if err := blocked.PutBlob(ctx, id, d, body); err == nil {
		t.Error("a write under a file")
	}
	if _, err := NewDir(file).Sessions(ctx); err == nil {
		t.Error("a listing of a file")
	}
	if ids, err := NewDir(filepath.Join(t.TempDir(), "none")).Sessions(ctx); err != nil || ids != nil {
		t.Errorf("a missing root: %v, %v", ids, err)
	}
	root := t.TempDir()
	st := NewDir(root)
	if err := os.MkdirAll(filepath.Join(root, id, d.Hex(), "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetBlob(ctx, id, d); err == nil || errors.Is(err, session.ErrNotFound) {
		t.Errorf("a body path that is a directory: %v", err)
	}
	if err := st.DeleteBlob(ctx, id, d); err == nil {
		t.Error("a body path that is a directory with entries was removed")
	}
	for name, call := range map[string]func() error{
		"get":            func() error { _, err := st.GetBlob(ctx, "bad", d); return err },
		"delete":         func() error { return st.DeleteBlob(ctx, id, "sha256:nope") },
		"delete session": func() error { return st.DeleteSession(ctx, "bad") },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: a bad id or digest was used", name)
		}
	}
}
