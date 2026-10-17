// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package filestore

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/arcastub"
)

// TestReadSource: a source is read at its owner's path with the bearer
// given, at ?version=N when it names one, inline and never through a
// redirect, with the files API's Content-Type; a 401, a 403 and a 404 are
// ErrSourceNotFound alike, a redirect and a body past the bound
// ErrSourceTooLarge, and any other refusal the files API's (spec 079).
func TestReadSource(t *testing.T) {
	s := arcastub.New(t)
	s.RedirectAbove(1 << 20)
	s.Put("me", "files/Reports/q 3.csv", arcastub.Object{Data: []byte("a,b\n"), ContentType: "text/csv"})
	s.PutVersion("me", "files/Reports/q 3.csv", 2, arcastub.Object{Data: []byte("old\n"), ContentType: "text/csv"})
	ctx := t.Context()
	src := session.AttachmentSource{Owner: "me", Path: "files/Reports/q 3.csv"}
	data, media, err := ReadSource(ctx, &http.Client{}, s.URL()+"/", "alice", src, 16)
	if err != nil || string(data) != "a,b\n" || media != "text/csv" {
		t.Fatalf("a read: %q %q %v", data, media, err)
	}
	r := s.Requests()[0]
	if r.Op != arcastub.OpRead || r.Path != "/v1/files/me/files/Reports/q%203.csv" || r.Query != "" || r.Header.Get("Authorization") != "Bearer alice" {
		t.Fatalf("the read is %+v", r)
	}
	src.Version = 2
	if data, _, err := ReadSource(ctx, &http.Client{}, s.URL(), "alice", src, 16); err != nil || string(data) != "old\n" || s.Requests()[1].Query != "version=2" {
		t.Fatalf("a version: %q %v %+v", data, err, s.Requests()[1])
	}
	src.Version = 0

	if _, _, err := ReadSource(ctx, &http.Client{}, s.URL(), "alice", src, 3); !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("a file past the bound: %v", err)
	}
	s.RedirectAbove(-1)
	if _, _, err := ReadSource(ctx, &http.Client{}, s.URL(), "alice", src, 16); !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("a redirect: %v", err)
	}
	for _, r := range s.Requests() {
		if r.Op == "bucket-read" {
			t.Fatal("a redirect was followed to the bucket")
		}
	}
	s.RedirectAbove(1 << 20)

	missing := session.AttachmentSource{Owner: "me", Path: "files/none.csv"}
	if _, _, err := ReadSource(ctx, &http.Client{}, s.URL(), "alice", missing, 16); !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("a missing file: %v", err)
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		s.Fail(arcastub.OpRead, arcastub.Failure{Status: status, Code: "refused"})
		if _, _, err := ReadSource(ctx, &http.Client{}, s.URL(), "alice", src, 16); !errors.Is(err, ErrSourceNotFound) {
			t.Fatalf("a %d: %v", status, err)
		}
	}
	s.Fail(arcastub.OpRead, arcastub.Failure{Status: http.StatusInternalServerError, Code: "internal"})
	_, _, err = ReadSource(ctx, &http.Client{}, s.URL(), "alice", src, 16)
	if fe, ok := errors.AsType[*harness.FileError](err); !ok || fe.Status != http.StatusInternalServerError || fe.Code != "internal" {
		t.Fatalf("a failure of the files API: %v", err)
	}

	for _, c := range []struct {
		root, token string
		client      *http.Client
	}{{"ftp://files.example", "alice", &http.Client{}}, {"", "alice", &http.Client{}}, {s.URL(), "", &http.Client{}}, {s.URL(), "alice", nil}} {
		if _, _, err := ReadSource(ctx, c.client, c.root, c.token, src, 16); err == nil || errors.Is(err, ErrSourceNotFound) {
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
	if _, _, err := ReadSource(ctx, &http.Client{}, chunked.URL, "alice", src, 16); !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("a body past the bound with no length: %v", err)
	}
	if data, media, err := ReadSource(ctx, &http.Client{}, chunked.URL, "alice", src, 20); err != nil || len(data) != 20 || media == "" {
		t.Fatalf("a body at the bound with no length: %d %q %v", len(data), media, err)
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	if _, _, err := ReadSource(ctx, &http.Client{}, gone.URL, "alice", src, 16); err == nil || errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("a files API that does not answer: %v", err)
	}
}
