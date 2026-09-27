// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package dir

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"latere.ai/x/topos/internal/blob"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/storetest"
)

// TestDirStoreConformanceWithOutsideBlobs: the store passes the suite
// with its blob bodies in a blob store of their own.
func TestDirStoreConformanceWithOutsideBlobs(t *testing.T) {
	storetest.Run(t, func(t *testing.T) session.Store {
		st, err := OpenWith(t.TempDir(), Options{Blobs: blob.NewDir(t.TempDir())})
		if err != nil {
			t.Fatal(err)
		}
		return st
	})
}

// TestOutsideBlobs: with a blob store the bodies live there and not in
// the session's directory, a body that does not hash to its digest is
// ErrCorrupt, a delete removes the directory before the bodies, and the
// sweep removes the bodies a failed delete left once the grace passed.
func TestOutsideBlobs(t *testing.T) {
	ctx := t.Context()
	data, outside := t.TempDir(), t.TempDir()
	blobs := &failing{Blobs: blob.NewDir(outside)}
	st, err := OpenWith(data, Options{Blobs: blobs})
	if err != nil {
		t.Fatal(err)
	}
	s := storetest.NewSession()
	body := []byte("the resolved agent")
	d := session.DigestOf(body)
	if err := st.Create(ctx, s, map[session.Digest][]byte{d: body}); err != nil {
		t.Fatal(err)
	}
	other, err := st.PutBlob(ctx, s.ID, bytes.NewReader([]byte("a raw response")))
	if err != nil {
		t.Fatal(err)
	}
	for _, dg := range []session.Digest{d, other} {
		if _, err := os.Stat(filepath.Join(outside, s.ID, dg.Hex())); err != nil {
			t.Fatalf("the body of %s outside: %v", dg, err)
		}
		if _, err := os.Stat(blobPath(st.dir(s.ID), dg)); !os.IsNotExist(err) {
			t.Fatalf("the body of %s is in the session's directory too: %v", dg, err)
		}
	}
	rc, err := st.Blob(ctx, s.ID, d)
	if err != nil {
		t.Fatal(err)
	}
	got, rerr := io.ReadAll(rc)
	if err := errors.Join(rerr, rc.Close()); err != nil || string(got) != string(body) {
		t.Fatalf("read back %q, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(outside, s.ID, other.Hex()), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Blob(ctx, s.ID, other); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("a tampered body: %v", err)
	}

	blobs.failDelete = true
	if err := st.Delete(ctx, s.ID); err == nil {
		t.Fatal("a delete whose bodies could not go reported nothing")
	}
	if _, err := st.Get(ctx, s.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("the session after its delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, s.ID)); err != nil {
		t.Fatalf("the bodies the failed delete left: %v", err)
	}
	blobs.failDelete = false
	if err := st.SweepBlobs(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, s.ID)); err != nil {
		t.Fatalf("a sweep within the grace removed the bodies: %v", err)
	}
	if err := st.SweepBlobs(ctx, time.Now().Add(session.SweepGrace+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, s.ID)); !os.IsNotExist(err) {
		t.Fatalf("the sweep left the orphaned bodies: %v", err)
	}
}

// failing is a blob store whose session deletes fail while failDelete
// is set.
type failing struct {
	session.Blobs
	failDelete bool
}

func (f *failing) DeleteSession(ctx context.Context, id string) error {
	if f.failDelete {
		return errors.New("the object store is down")
	}
	return f.Blobs.DeleteSession(ctx, id)
}
