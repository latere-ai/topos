// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build postgres

package postgres

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"latere.ai/x/pkg/s3/s3test"

	"latere.ai/x/topos/internal/blob"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/storetest"
)

// TestPostgresStoreConformanceWithObjectBlobs: the store passes the
// suite with its blob bodies in an object store.
func TestPostgresStoreConformanceWithObjectBlobs(t *testing.T) {
	storetest.Run(t, func(t *testing.T) session.Store {
		return fresh(t, Options{Poll: 200 * time.Millisecond, Blobs: blob.NewS3(s3test.New(t, "bucket").Client(true), "topos")})
	})
}

// TestPostgresBlobsOutsideTheDatabase: a body kept outside has a row of
// location object and no body, reads back verified, and a delete removes
// the rows before the bodies, whose leftovers the sweep removes.
func TestPostgresBlobsOutsideTheDatabase(t *testing.T) {
	ctx := t.Context()
	outside := t.TempDir()
	st := fresh(t, Options{Blobs: blob.NewDir(outside)})
	s := storetest.NewSession()
	if err := st.Create(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	d, err := st.PutBlob(ctx, s.ID, bytes.NewReader([]byte("a raw response")))
	if err != nil {
		t.Fatal(err)
	}
	var location string
	var body []byte
	if err := st.pool.QueryRow(ctx, `SELECT location, body FROM blobs WHERE session_id = $1 AND digest = $2`, s.ID, string(d)).Scan(&location, &body); err != nil || location != "object" || body != nil {
		t.Fatalf("the row: %q %q, %v", location, body, err)
	}
	rc, err := st.Blob(ctx, s.ID, d)
	if err != nil {
		t.Fatal(err)
	}
	got, rerr := io.ReadAll(rc)
	if err := errors.Join(rerr, rc.Close()); err != nil || string(got) != "a raw response" {
		t.Fatalf("read back %q, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(outside, s.ID, d.Hex()), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Blob(ctx, s.ID, d); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("a tampered body: %v", err)
	}
	if err := st.Delete(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, s.ID)); !os.IsNotExist(err) {
		t.Fatalf("the bodies after the delete: %v", err)
	}
	orphan := storetest.NewSession()
	if err := blob.NewDir(outside).PutBlob(ctx, orphan.ID, d, []byte("a raw response")); err != nil {
		t.Fatal(err)
	}
	if err := st.SweepBlobs(ctx, time.Now().Add(session.SweepGrace+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, orphan.ID)); !os.IsNotExist(err) {
		t.Fatalf("the sweep left the bodies of a session the database never held: %v", err)
	}
}
