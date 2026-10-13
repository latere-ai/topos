// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/machine/cella"
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

// TestASandboxIsRestoredFromTheSessionsFolder: over the stub Cella and
// the stub files API, a hosted session with a folder keeps its working
// directory under work/ at the turn's end, with the session's own token,
// a command's files and their modes included and a dependency directory
// left out; its sandbox fails and is replaced, and the new sandbox has the
// files back before its first command, recorded restored from the folder,
// and the model reads it (spec 073).
func TestASandboxIsRestoredFromTheSessionsFolder(t *testing.T) {
	arca := arcastub.New(t)
	const folder = "files/sessions/work"
	c := newCloudOf(t, writerAgent, func(s *session.Session) {
		s.Folder = &session.Folder{URL: arca.URL(), Owner: "o", Path: folder}
	})
	c.o.FilesAudience = "arca"
	c.creds = &issued{life: 15 * time.Minute}
	c.drive("Build the site.",
		tool(t, "toolu_1", "write", map[string]any{"path": "src/app.js", "content": "console.log('tides');\n"}),
		bash("toolu_2", "mkdir -p dist node_modules/dep && echo '<h1>Tides</h1>' > dist/index.html && echo dep > node_modules/dep/x.js && printf '#!/bin/sh\\necho hi\\n' > run.sh && chmod 755 run.sh"),
		said("Built."),
	)
	got := arca.Paths("o", folder+"/work/")
	want := []string{folder + "/work/dist/index.html", folder + "/work/run.sh", folder + "/work/src/app.js"}
	if !slices.Equal(got, want) {
		t.Fatalf("the folder's work/ holds %v, want %v\n%s", got, want, c.dump())
	}
	if o, _ := arca.Object("o", folder+"/work/dist/index.html"); !strings.HasPrefix(o.Token, "arca-session-") || string(o.Data) != "<h1>Tides</h1>\n" {
		t.Fatalf("the copy's file %+v", o)
	}
	name := cella.SandboxName(c.s.ID)
	c.cella.SetFailed(name, "OOMKilled")
	c.drive("Show me the site.", bash("toolu_3", "cat dist/index.html; ls -l run.sh; ls node_modules 2>&1 || true"), said("The site is back."))
	results := c.results()
	last := results[len(results)-1]
	if !strings.Contains(last, "<h1>Tides</h1>") || !strings.Contains(last, "-rwxr-xr-x") || strings.Contains(last, "dep") {
		t.Fatalf("the new sandbox reads %q", last)
	}
	machines := c.events(session.TypeSessionMachine)
	var restored session.SessionMachine
	if len(machines) != 2 || machines[1].Decode(&restored) != nil || restored.Reason != session.MachineRestored || restored.Restore == nil {
		t.Fatalf("session.machine events %s", c.dump())
	}
	if r := restored.Restore; r.Source != session.RestoreFromFolder || r.Copy == nil || r.Copy.Files != 3 || r.Copy.Turn != 1 || r.Files != 0 || r.Omitted != 0 {
		t.Fatalf("the restore record %+v, copy %+v", r, r.Copy)
	}
	if n := len(c.events(session.TypeSessionError)); n != 0 {
		t.Fatalf("%d session.error events\n%s", n, c.dump())
	}
	if !c.lastSaid("was restored from the copy this conversation keeps of it, as it was saved at the end of turn 1: 3 files.") {
		t.Fatal("the model does not read that the working directory came back from the copy")
	}
}
