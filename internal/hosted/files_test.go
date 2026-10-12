// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/arcastub"
)

// TestASessionsFolderIsWrittenWithItsOwnToken: a session with a folder
// of a files store gets its folder in its harness, written with the
// session's own token for the files audience and the workload session; a
// session with none gets none; and one with a folder on a runner with no
// files audience or no token source is files_unavailable (spec 068).
func TestASessionsFolderIsWrittenWithItsOwnToken(t *testing.T) {
	arca := arcastub.New(t)
	creds := &issued{life: 15 * time.Minute}
	ctx := runner.WithTokens(t.Context(), runner.NewTokenSource(creds, nil, nil))
	b := builder{o: Options{FilesAudience: "arca"}}
	s := session.Session{ID: session.NewID(session.PrefixSession), Folder: &session.Folder{URL: arca.URL(), Owner: "o", Path: "files/sessions/s"}}
	f, err := b.files(ctx, s)
	if err != nil || f == nil {
		t.Fatalf("files: %v", err)
	}
	if _, err := f.Put(ctx, "results/1/a.txt", "text/plain", 1, bytes.NewReader([]byte("a"))); err != nil {
		t.Fatal(err)
	}
	if o, ok := arca.Object("o", "files/sessions/s/results/1/a.txt"); !ok || o.Token != "arca-session-1" {
		t.Fatalf("the file %+v %t", o, ok)
	}
	if f, err := b.files(ctx, session.Session{}); err != nil || f != nil {
		t.Fatalf("a session with no folder: %v %v", f, err)
	}
	for name, c := range map[string]struct {
		b   builder
		ctx bool
		s   session.Session
	}{
		"no audience":     {builder{}, true, s},
		"no token source": {b, false, s},
		"a bad folder":    {b, true, session.Session{Folder: &session.Folder{URL: "ftp://x", Owner: "o", Path: "files/p"}}},
	} {
		use := ctx
		if !c.ctx {
			use = t.Context()
		}
		_, err := c.b.files(use, c.s)
		if se, ok := errors.AsType[*runner.SetupError](err); !ok || se.Code != CodeFilesUnavailable {
			t.Errorf("%s: %v", name, err)
		}
	}
}
