// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
	child, err = session.Fork(ctx, f.store, child, nil, f.s.ID, evs[:seq])
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
	if n := f.countIn(t.Context(), child.ID, session.TypeSessionError); n != 0 {
		t.Fatalf("%d session errors", n)
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
