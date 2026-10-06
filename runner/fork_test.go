// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// forkParent runs one turn of the fixture's session that writes notes.txt
// into its working directory, ends the session expired, and forks it at
// that turn into a session working in a directory of its own, which it
// returns with the fork point's checkpoint.
func forkParent(t *testing.T, f *fixture) (session.Session, string, session.CheckpointRef) {
	t.Helper()
	ctx := t.Context()
	write(t, filepath.Join(f.work, "notes.txt"), "draft one\n")
	f.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: "Noted the first draft."}))
	f.message(ctx, "Turn one.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	f.send(ctx, session.TypeSessionStatus, session.SessionStatus{Status: session.StatusEnded, StopReason: session.StopExpired})
	evs, err := f.store.Events(ctx, f.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	cp, _ := checkpointOf(evs, 1)
	if cp == nil {
		t.Fatal("the parent's turn kept no checkpoint")
	}
	seq, err := session.ForkPoint(evs, nil)
	if err != nil {
		t.Fatal(err)
	}
	child := session.New(f.s.Agent, f.s.Initiator, session.RunnerExternal, session.Machine{Kind: machine.KindHost}, t0)
	child, err = session.Fork(ctx, f.store, child, nil, f.s, evs[:seq])
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(filepath.Dir(f.work), "fork")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	f.works[child.ID] = work
	// The parent's directory moves on; the fork reads none of it.
	write(t, filepath.Join(f.work, "notes.txt"), "a later edit\n")
	return child, work, *cp
}

// continueFork sends the fork its first message and drives it, holding
// the model's request to the parent's history.
func continueFork(t *testing.T, f *fixture, child session.Session) []session.Event {
	t.Helper()
	ctx := t.Context()
	f.stub.Script(model, luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{{Type: ir.BlockText, Text: "Continuing."}}, StopReason: ir.StopEndTurn},
		Expect: func(r *ir.Request) error {
			var said []string
			for _, m := range r.Messages {
				for _, b := range m.Blocks {
					said = append(said, b.Text)
				}
			}
			all := strings.Join(said, "\n")
			for _, want := range []string{"Turn one.", "Noted the first draft.", "Turn two."} {
				if !strings.Contains(all, want) {
					return errors.New("the fork's first request lacks " + want)
				}
			}
			return nil
		}})
	f.sendTo(ctx, child.ID, session.TypeUserMessage, session.UserMessage{Sender: child.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: "Turn two."}}})
	if _, err := f.r.Drive(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	evs, err := f.store.Events(ctx, child.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// ownMachine is the first session.machine a fork appended itself.
func ownMachine(t *testing.T, s session.Session, evs []session.Event) session.SessionMachine {
	t.Helper()
	for _, e := range evs {
		if e.Type == session.TypeSessionMachine && !s.Copied(e) {
			var p session.SessionMachine
			if err := e.Decode(&p); err != nil {
				t.Fatal(err)
			}
			return p
		}
	}
	t.Fatal("the fork recorded no machine of its own")
	return session.SessionMachine{}
}

// gitIn runs git against a bare repository and answers its output.
func gitIn(t *testing.T, repo string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), "git", args...)
	cmd.Env = append(os.Environ(), "GIT_DIR="+repo)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// TestAForkRestoresTheForkPointsFiles: a fork of an expired session
// opens a machine of its own, puts the fork point's files in it from the
// parent's session repository, records the machine restored with the
// checkpoint as its own of that turn, sees the parent's history in its
// first request, and chains its next checkpoint to the restored one.
func TestAForkRestoresTheForkPointsFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	f := setup(t)
	f.r.o.CheckpointDir = filepath.Join(filepath.Dir(f.work), "checkpoints")
	child, work, cp := forkParent(t, f)
	evs := continueFork(t, f, child)
	if b, err := os.ReadFile(filepath.Join(work, "notes.txt")); err != nil || string(b) != "draft one\n" {
		t.Fatalf("the fork's notes.txt is %q, %v; want the fork point's", b, err)
	}
	m := ownMachine(t, child, evs)
	if m.Reason != "restored" || m.Checkpoint == nil || m.Checkpoint.Commit != cp.Commit || m.Checkpoint.Ref != "refs/topos/checkpoints/"+child.ID+"/1" {
		t.Fatalf("the fork's machine: reason %q, checkpoint %+v; want restored at %s", m.Reason, m.Checkpoint, cp.Commit)
	}
	next, _ := checkpointOf(evs, 2)
	if next == nil {
		t.Fatal("the fork's turn kept no checkpoint")
	}
	repo := filepath.Join(f.r.o.CheckpointDir, child.ID+".git")
	if parent, err := gitIn(t, repo, "rev-parse", next.Commit+"^"); err != nil || parent != cp.Commit {
		t.Fatalf("the fork's checkpoint chains to %q (%v), want the restored %s", parent, err, cp.Commit)
	}
}

// TestAForkWithoutItsCheckpointStartsFresh: a fork whose runner cannot
// reach the fork point's checkpoint, as a hosted sandbox's left with it,
// records its first machine attached with no checkpoint, starts on an
// empty directory, and starts a chain of its own.
func TestAForkWithoutItsCheckpointStartsFresh(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	f := setup(t)
	f.r.o.CheckpointDir = filepath.Join(filepath.Dir(f.work), "checkpoints")
	child, work, _ := forkParent(t, f)
	// The fork runs where the parent's checkpoints are not.
	f.r.o.CheckpointDir = filepath.Join(filepath.Dir(f.work), "elsewhere")
	evs := continueFork(t, f, child)
	if _, err := os.Stat(filepath.Join(work, "notes.txt")); !os.IsNotExist(err) {
		t.Fatalf("a file appeared without a checkpoint to restore: %v", err)
	}
	if m := ownMachine(t, child, evs); m.Reason != "attached" || m.Checkpoint != nil {
		t.Fatalf("the fork's machine: reason %q, checkpoint %+v; want attached with none", m.Reason, m.Checkpoint)
	}
	next, _ := checkpointOf(evs, 2)
	if next == nil {
		t.Fatal("the fork's turn kept no checkpoint")
	}
	repo := filepath.Join(f.r.o.CheckpointDir, child.ID+".git")
	if parent, err := gitIn(t, repo, "rev-parse", "--verify", "--quiet", next.Commit+"^"); err == nil {
		t.Fatalf("the fork's first checkpoint chains to %s, which its repository never held", parent)
	}
	missing := f.errorsIn(t.Context(), child.ID)
	if len(missing) != 1 || missing[0].Code != "checkpoint_missing" || missing[0].Retryable || !strings.Contains(missing[0].Detail, "kept on the machine of that session alone") {
		t.Fatalf("session errors %+v, want one checkpoint_missing saying why", missing)
	}
}

// TestAForkOfNothingStartsFresh: a fork before its parent's opening
// message copies nothing and starts with its own message, so its first
// machine opens on a directory of its own with none of the parent's
// files, attached with no checkpoint and no checkpoint_missing.
func TestAForkOfNothingStartsFresh(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	f := setup(t)
	f.r.o.CheckpointDir = filepath.Join(filepath.Dir(f.work), "checkpoints")
	ctx := t.Context()
	write(t, filepath.Join(f.work, "notes.txt"), "draft one\n")
	f.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: "Noted the first draft."}))
	f.message(ctx, "Turn one.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	evs, err := f.store.Events(ctx, f.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	at, err := session.ForkBefore(evs, 1)
	if err != nil || at != 0 {
		t.Fatalf("the fork point before the opening message is %d, %v", at, err)
	}
	msg, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: f.s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: "Turn one, put another way."}}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	child := session.New(f.s.Agent, f.s.Initiator, session.RunnerExternal, session.Machine{Kind: machine.KindHost}, t0)
	if child, err = session.Fork(ctx, f.store, child, nil, f.s, evs[:at], msg); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(filepath.Dir(f.work), "fork")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	f.works[child.ID] = work
	f.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: "Starting over."}))
	if _, err := f.r.Drive(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	cevs, err := f.store.Events(ctx, child.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if m := ownMachine(t, child, cevs); m.Reason != "attached" || m.Checkpoint != nil {
		t.Fatalf("the fork's machine: reason %q, checkpoint %+v; want attached with none", m.Reason, m.Checkpoint)
	}
	if _, err := os.Stat(filepath.Join(work, "notes.txt")); !os.IsNotExist(err) {
		t.Fatalf("the parent's file reached a fork that copied nothing: %v", err)
	}
	if errs := f.errorsIn(ctx, child.ID); len(errs) != 0 {
		t.Fatalf("session errors %+v, want none", errs)
	}
}

// TestAnEndOnIdleSessionForksAtItsTurn: a session created with
// end_on_idle ends completed straight from running when its turn ends,
// with no idle between; it forks at that end, the fork waits idle
// end_turn, restores the turn's files from the end's checkpoint, and its
// first request carries the whole conversation.
func TestAnEndOnIdleSessionForksAtItsTurn(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	f := setup(t)
	ctx := t.Context()
	f.r.o.CheckpointDir = filepath.Join(filepath.Dir(f.work), "checkpoints")
	parent := session.New(f.s.Agent, f.s.Initiator, session.RunnerExternal, session.Machine{Kind: machine.KindHost}, t0)
	parent.EndOnIdle = true
	if err := f.store.Create(ctx, parent, nil); err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(f.work)
	f.works[parent.ID] = filepath.Join(base, "parent")
	write(t, filepath.Join(f.works[parent.ID], "notes.txt"), "draft one\n")
	f.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: "Noted the first draft."}))
	f.sendTo(ctx, parent.ID, session.TypeUserMessage, session.UserMessage{Sender: parent.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: "Turn one."}}})
	out, err := f.r.Drive(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != session.StatusEnded || out.StopReason != session.StopCompleted {
		t.Fatalf("the turn ended %s %s, want ended completed", out.Status, out.StopReason)
	}
	evs, err := f.store.Events(ctx, parent.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var statuses []session.Status
	for _, e := range evs {
		var p session.SessionStatus
		if e.Type == session.TypeSessionStatus && e.Thread == "" && e.Decode(&p) == nil {
			statuses = append(statuses, p.Status)
		}
	}
	if !slices.Equal(statuses, []session.Status{session.StatusRunning, session.StatusEnded}) {
		t.Fatalf("the parent's statuses are %v, want running then ended", statuses)
	}
	cp, _ := checkpointOf(evs, 1)
	if cp == nil {
		t.Fatal("the parent's turn kept no checkpoint")
	}
	seq, err := session.ForkPoint(evs, nil)
	if err != nil || seq != uint64(len(evs)) {
		t.Fatalf("the fork point is %d, %v; want the end at %d", seq, err, len(evs))
	}
	child := session.New(f.s.Agent, f.s.Initiator, session.RunnerExternal, session.Machine{Kind: machine.KindHost}, t0)
	if child, err = session.Fork(ctx, f.store, child, nil, parent, evs[:seq]); err != nil {
		t.Fatal(err)
	}
	if child.Status != session.StatusIdle || child.StopReason != session.StopEndTurn {
		t.Fatalf("the fork is %s %s, want idle end_turn", child.Status, child.StopReason)
	}
	work := filepath.Join(base, "fork")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	f.works[child.ID] = work
	got := continueFork(t, f, child)
	if b, err := os.ReadFile(filepath.Join(work, "notes.txt")); err != nil || string(b) != "draft one\n" {
		t.Fatalf("the fork's notes.txt is %q, %v; want the fork point's", b, err)
	}
	if m := ownMachine(t, child, got); m.Reason != "restored" || m.Checkpoint == nil || m.Checkpoint.Commit != cp.Commit {
		t.Fatalf("the fork's machine: reason %q, checkpoint %+v; want restored at %s", m.Reason, m.Checkpoint, cp.Commit)
	}
}

// errorsIn are the session.error payloads of the session id's log.
func (f *fixture) errorsIn(ctx context.Context, id string) []session.SessionError {
	f.t.Helper()
	evs, err := f.store.Events(ctx, id, 1, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []session.SessionError
	for _, e := range evs {
		if e.Type != session.TypeSessionError {
			continue
		}
		var p session.SessionError
		if err := e.Decode(&p); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// gitHostToken is the credential the test's git host takes, sent by the
// machines' git as a sandbox's sends its placeholder.
const gitHostToken = "Bearer runner-test"

// gitHost serves the bare repositories under base/host over git's own
// http backend, a request with gitHostToken reading and writing, one with
// no credential refused 401 as a private repository's is, but reading a
// repository public names, and points every machine of f at it with the
// header in their git configuration. It answers the host's URL.
func gitHost(t *testing.T, f *fixture, base string, public ...string) string {
	t.Helper()
	bin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not on PATH")
	}
	backend := &cgi.Handler{Path: bin, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + filepath.Join(base, "host"), "GIT_HTTP_EXPORT_ALL=1"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		read := r.URL.Query().Get("service") == "git-upload-pack" || strings.HasSuffix(r.URL.Path, "/git-upload-pack")
		if r.Header.Get("Authorization") == gitHostToken || (read && slices.Contains(public, name)) {
			backend.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	home := filepath.Join(base, "home")
	write(t, filepath.Join(home, ".gitconfig"), "[http \""+srv.URL+"/\"]\n\textraHeader = Authorization: "+gitHostToken+"\n")
	f.environ = append(f.environ, "HOME="+home)
	return srv.URL
}

// gitHostRepo makes a bare repository with one commit on main under
// base/host, the git host of a test, and answers its URL on host and its
// path.
func gitHostRepo(t *testing.T, base, host, name string) (string, string) {
	t.Helper()
	bare := filepath.Join(base, "host", name)
	seed := filepath.Join(base, "seed-"+name)
	for _, args := range [][]string{
		{"init", "--quiet", "--bare", "-b", "main", bare},
		{"-C", bare, "config", "http.receivepack", "true"},
		{"init", "--quiet", "-b", "main", seed},
		{"-C", seed, "commit", "--quiet", "--allow-empty", "-m", "start"},
		{"-C", seed, "push", "--quiet", bare, "HEAD:main"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=p", "GIT_AUTHOR_EMAIL=p@example.com", "GIT_COMMITTER_NAME=p", "GIT_COMMITTER_EMAIL=p@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return host + "/" + name, bare
}

// forkFromRepository runs one turn of a session that works in the
// repository url and writes notes.txt in its working directory, ends it
// expired, deletes its working directory as a sandbox goes at a hosted
// session's end, and forks it at that turn into a session that works in
// the repository fork in a directory of its own.
func forkFromRepository(t *testing.T, f *fixture, url, fork string) (session.Session, string, session.CheckpointRef) {
	t.Helper()
	ctx := t.Context()
	parent := session.New(f.s.Agent, f.s.Initiator, session.RunnerExternal, session.Machine{Kind: machine.KindHost}, t0)
	parent.Resources = []session.Resource{{Type: session.ResourceRepository, URL: url}}
	if err := f.store.Create(ctx, parent, nil); err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(f.work)
	work := filepath.Join(base, "parent")
	f.works[parent.ID] = work
	write(t, filepath.Join(work, "notes.txt"), "draft one\n")
	f.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: "Noted the first draft."}))
	f.sendTo(ctx, parent.ID, session.TypeUserMessage, session.UserMessage{Sender: parent.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: "Turn one."}}})
	if _, err := f.r.Drive(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	f.sendTo(ctx, parent.ID, session.TypeSessionStatus, session.SessionStatus{Status: session.StatusEnded, StopReason: session.StopExpired})
	if err := os.RemoveAll(work); err != nil {
		t.Fatal(err)
	}
	evs, err := f.store.Events(ctx, parent.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	cp, _ := checkpointOf(evs, 1)
	if cp == nil {
		t.Fatal("the parent's turn kept no checkpoint")
	}
	seq, err := session.ForkPoint(evs, nil)
	if err != nil {
		t.Fatal(err)
	}
	child := session.New(f.s.Agent, f.s.Initiator, session.RunnerExternal, session.Machine{Kind: machine.KindHost}, t0)
	child.Resources = []session.Resource{{Type: session.ResourceRepository, URL: fork}}
	if child, err = session.Fork(ctx, f.store, child, nil, parent, evs[:seq]); err != nil {
		t.Fatal(err)
	}
	f.works[child.ID] = filepath.Join(base, "fork-"+child.ID)
	if err := os.MkdirAll(f.works[child.ID], 0o755); err != nil {
		t.Fatal(err)
	}
	return child, f.works[child.ID], *cp
}

// TestAForkRestoresFromTheRepository: a session that works in a
// repository on the checkpoint host keeps its checkpoint there, so a fork
// whose parent's working directory is gone fetches the fork point's
// checkpoint by its id into a fresh clone, restores its files, records
// the machine restored, chains its next checkpoint to it and keeps that
// one at the repository as its own latest.
func TestAForkRestoresFromTheRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	f := setup(t)
	base := filepath.Dir(f.work)
	host := gitHost(t, f, base)
	url, bare := gitHostRepo(t, base, host, "app.git")
	f.r.o.CheckpointHost = host + "/"
	child, work, cp := forkFromRepository(t, f, url, url)
	if cp.Remote != url {
		t.Fatalf("the parent's checkpoint %+v, want it kept at %s", cp, url)
	}
	parent := child.Parent.SessionID
	if got, err := gitIn(t, bare, "rev-parse", "refs/topos/checkpoints/"+parent+"/latest"); err != nil || got != cp.Commit {
		t.Fatalf("the repository's latest of the parent is %q (%v), want %s", got, err, cp.Commit)
	}
	evs := continueFork(t, f, child)
	if b, err := os.ReadFile(filepath.Join(work, "notes.txt")); err != nil || string(b) != "draft one\n" {
		t.Fatalf("the fork's notes.txt is %q, %v; want the fork point's", b, err)
	}
	m := ownMachine(t, child, evs)
	if m.Reason != "restored" || m.Checkpoint == nil || m.Checkpoint.Commit != cp.Commit || len(m.Repositories) != 1 {
		t.Fatalf("the fork's machine: reason %q, checkpoint %+v, repositories %+v; want restored at %s over its repository", m.Reason, m.Checkpoint, m.Repositories, cp.Commit)
	}
	next, _ := checkpointOf(evs, 2)
	if next == nil || next.Remote != url {
		t.Fatalf("the fork's checkpoint %+v, want it kept at %s", next, url)
	}
	if got, err := gitIn(t, bare, "rev-parse", next.Commit+"^"); err != nil || got != cp.Commit {
		t.Fatalf("the fork's checkpoint chains to %q (%v), want the restored %s", got, err, cp.Commit)
	}
	if got, err := gitIn(t, bare, "rev-parse", "refs/topos/checkpoints/"+child.ID+"/latest"); err != nil || got != next.Commit {
		t.Fatalf("the repository's latest of the fork is %q (%v), want %s", got, err, next.Commit)
	}
	if errs := f.errorsIn(t.Context(), child.ID); len(errs) != 0 {
		t.Fatalf("session errors %+v", errs)
	}
}

// TestAForkWithoutItsKeptCheckpointSaysSo: a fork whose parent kept its
// checkpoint on its machine alone, off the checkpoint host or in a public
// repository, which never gets one, one whose copied checkpoint names a
// repository that is not the fork's own, and one whose repository lost
// the checkpoint, each start on their repository, recorded attached with
// a checkpoint_missing beside the machine that says why, and run their
// turn.
func TestAForkWithoutItsKeptCheckpointSaysSo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	for _, c := range []struct {
		name, why string
		host      bool
		public    bool
		other     bool
		lose      bool
	}{
		{name: "off the checkpoint host", why: "kept on the machine of that session alone"},
		{name: "a public repository", why: "kept on the machine of that session alone", host: true, public: true},
		{name: "another repository", why: "not this session's own repository", host: true, other: true},
		{name: "lost at the repository", why: "does not give the checkpoint", host: true, lose: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			base := filepath.Dir(f.work)
			var public []string
			if c.public {
				public = []string{"app.git"}
			}
			host := gitHost(t, f, base, public...)
			url, bare := gitHostRepo(t, base, host, "app.git")
			fork := url
			if c.other {
				fork, _ = gitHostRepo(t, base, host, "other.git")
			}
			if c.host {
				f.r.o.CheckpointHost = host
			}
			child, work, cp := forkFromRepository(t, f, url, fork)
			if refs, err := gitIn(t, bare, "for-each-ref", "--format=%(refname)", "refs/topos"); c.public && (err != nil || refs != "") {
				t.Fatalf("the public repository holds %q (%v)", refs, err)
			}
			if c.lose {
				if _, err := gitIn(t, bare, "update-ref", "-d", "refs/topos/checkpoints/"+child.Parent.SessionID+"/latest"); err != nil {
					t.Fatal(err)
				}
				if _, err := gitIn(t, bare, "-c", "gc.reflogExpireUnreachable=now", "gc", "--quiet", "--prune=now"); err != nil {
					t.Fatal(err)
				}
			}
			evs := continueFork(t, f, child)
			if _, err := os.Stat(filepath.Join(work, "notes.txt")); !os.IsNotExist(err) {
				t.Fatalf("notes.txt appeared without its checkpoint: %v", err)
			}
			if m := ownMachine(t, child, evs); m.Reason != "attached" || m.Checkpoint != nil || len(m.Repositories) != 1 {
				t.Fatalf("the fork's machine: %+v; want attached over its repository", m)
			}
			missing := f.errorsIn(t.Context(), child.ID)
			if len(missing) != 1 || missing[0].Code != "checkpoint_missing" || !strings.Contains(missing[0].Detail, cp.Commit) || !strings.Contains(missing[0].Detail, c.why) {
				t.Fatalf("session errors %+v, want one checkpoint_missing naming %s and saying %q", missing, cp.Commit, c.why)
			}
			if next, _ := checkpointOf(evs, 2); next == nil {
				t.Fatal("the fork's turn kept no checkpoint")
			}
		})
	}
}

// countIn counts the events of a type in the session id's log.
func (f *fixture) countIn(ctx context.Context, id string, typ session.Type) int {
	f.t.Helper()
	evs, err := f.store.Events(ctx, id, 1, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}
