// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/test/stubs/cellastub"
)

// restarts collects the restarts a machine told its hook of.
type restarts struct {
	mu   sync.Mutex
	told []machine.Restart
	fail error
}

func (r *restarts) hook(_ context.Context, rs machine.Restart) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.told = append(r.told, rs)
	return r.fail
}

func (r *restarts) all() []machine.Restart {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]machine.Restart(nil), r.told...)
}

// loseHelper removes the helper, which lives outside the workspace and
// does not outlive a restart of the sandbox.
func (f fixture) loseHelper(t *testing.T) {
	t.Helper()
	if err := os.Remove(filepath.Join(f.o.Dir, "bin", "topos-machine")); err != nil {
		t.Fatal(err)
	}
}

// content is a file of the machine, read whole.
func content(t *testing.T, m *Machine, p string) string {
	t.Helper()
	b, err := read(t, m, p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return b
}

// TestAFailedSandboxIsStartedAndNeverDeleted: on a core that starts a
// failed sandbox whose workspace is kept, an open and a call that find
// the session's sandbox failed start it again. It keeps its id and its
// files, gets its helper back, is reported restarted with the cause Cella
// named, and no delete is ever sent (spec 009).
func TestAFailedSandboxIsStartedAndNeverDeleted(t *testing.T) {
	f := open(t, func(s *cellastub.Server, _ *Options) { s.RestartFailed(true) })
	id := f.m.Info().ID
	if err := f.m.WriteFile(t.Context(), "work.txt", strings.NewReader("seventy minutes of work"), 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.m.Restarted(); ok || f.m.Created() != true {
		t.Fatalf("a first open: restarted %v, created %v", ok, f.m.Created())
	}

	// The next runner's open finds the sandbox failed for memory.
	f.stub.SetFailed(id, "OOMKilled")
	f.loseHelper(t)
	starts := f.stub.Count(cellastub.OpStart)
	again, err := Open(t.Context(), f.o)
	if err != nil {
		t.Fatal(err)
	}
	rs, ok := again.Restarted()
	if !ok || !rs.OutOfMemory || again.Created() || again.Info().ID != id {
		t.Fatalf("an open of a failed sandbox: %s, created %v, restarted %+v %v; want %s restarted for memory", again.Info().ID, again.Created(), rs, ok, id)
	}
	if got := content(t, again, "work.txt"); got != "seventy minutes of work" {
		t.Errorf("the workspace after the start = %q", got)
	}
	if err := execErr(t, again); err != nil {
		t.Errorf("a command after the start, whose helper the start lost: %v", err)
	}
	if n := f.stub.Count(cellastub.OpStart) - starts; n != 1 {
		t.Errorf("%d starts for one failed sandbox", n)
	}

	// A call that finds it failed starts it too, and tells the hook, once
	// for each failure and with no cause Cella did not name.
	var told restarts
	again.OnRestart(told.hook)
	for name, call := range map[string]func() error{
		"exec":  func() error { return execErr(t, again) },
		"stat":  func() error { return statErr(t, again, "work.txt") },
		"write": func() error { return again.WriteFile(t.Context(), "w.txt", strings.NewReader("w"), 0) },
	} {
		before := len(told.all())
		f.stub.SetFailed(id, "Exited")
		f.loseHelper(t)
		if err := call(); err != nil {
			t.Errorf("%s on a failed sandbox: %v", name, err)
		}
		if sb, _ := f.stub.Sandbox(id); sb.Status.Phase != "Running" || again.Info().ID != id {
			t.Errorf("%s left the sandbox %s as %s", name, sb.Status.Phase, again.Info().ID)
		}
		if got := told.all()[before:]; len(got) != 1 || got[0].OutOfMemory {
			t.Errorf("%s told the hook %+v", name, got)
		}
	}
	// An open that found the sandbox running reports no restart.
	if third, err := Open(t.Context(), f.o); err != nil {
		t.Fatal(err)
	} else if _, ok := third.Restarted(); ok || third.Created() {
		t.Error("an open of a running sandbox reports a restart or a create")
	}
	// A hook that fails is the call's failure, and the sandbox stays up.
	told.fail = errors.New("the log refused the record")
	f.stub.SetFailed(id, "Exited")
	if err := execErr(t, again); !errors.Is(err, told.fail) {
		t.Errorf("a call whose hook failed: %v", err)
	}
	if sb, _ := f.stub.Sandbox(id); sb.Status.Phase != "Running" {
		t.Errorf("the sandbox after a failed hook is %s", sb.Status.Phase)
	}
	if n := f.stub.Count(cellastub.OpDelete); n != 0 {
		t.Fatalf("%d deletes of a sandbox the core starts again", n)
	}
	if got := content(t, again, "work.txt"); got != "seventy minutes of work" {
		t.Errorf("the workspace at the end = %q", got)
	}
}

// TestACoreThatStartsNoFailedSandbox: a core that answers phase_conflict
// to a start of a failed sandbox, an older one or any core for a failure
// that left no workspace, gets the old path. An open replaces the sandbox
// and reports it created, so the runner records the loss; a call marks
// the machine lost and deletes nothing. A start refused for another
// reason, and a sandbox gone when it is started, are not taken for it
// (spec 009).
func TestACoreThatStartsNoFailedSandbox(t *testing.T) {
	t.Run("an open replaces it", func(t *testing.T) {
		f := open(t)
		id := f.m.Info().ID
		if err := f.m.WriteFile(t.Context(), "work.txt", strings.NewReader("work"), 0); err != nil {
			t.Fatal(err)
		}
		f.stub.SetFailed(id, "OOMKilled")
		again, err := Open(t.Context(), f.o)
		if err != nil {
			t.Fatal(err)
		}
		if _, restarted := again.Restarted(); restarted || !again.Created() || again.Info().ID == id {
			t.Fatalf("an open of a failed sandbox the core does not start: %s, created %v, restarted %v", again.Info().ID, again.Created(), restarted)
		}
		if _, err := again.Stat(t.Context(), "work.txt"); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the replaced sandbox has the old files: %v", err)
		}
		// The refusal is asked for twice before the delete, so a sandbox
		// that failed again between a start and its read is not deleted.
		if starts, deletes := f.stub.Count(cellastub.OpStart), f.stub.Count(cellastub.OpDelete); starts != 2 || deletes != 1 {
			t.Errorf("%d starts and %d deletes, want 2 and 1", starts, deletes)
		}
		if err := again.Release(t.Context(), true); err != nil {
			t.Error(err)
		}
	})
	t.Run("a failure with no workspace on a core that starts the others", func(t *testing.T) {
		f := open(t, func(s *cellastub.Server, _ *Options) { s.RestartFailed(true) })
		id := f.m.Info().ID
		f.stub.SetFailed(id, "VolumeMissing")
		f.stub.Fail(cellastub.OpStart, cellastub.Failure{Status: 409, Code: "phase_conflict", Times: 2})
		again, err := Open(t.Context(), f.o)
		if err != nil {
			t.Fatal(err)
		}
		if !again.Created() || again.Info().ID == id {
			t.Errorf("an open of it: %s, created %v", again.Info().ID, again.Created())
		}
		if err := again.Release(t.Context(), true); err != nil {
			t.Error(err)
		}
	})
	t.Run("a start that raced another is a restart", func(t *testing.T) {
		f := open(t, func(s *cellastub.Server, _ *Options) { s.RestartFailed(true) })
		id := f.m.Info().ID
		f.stub.SetFailed(id, "OOMKilled")
		// The first start is refused as one that lost a race is; the
		// sandbox still reads Failed, and the second start is taken.
		f.stub.Fail(cellastub.OpStart, cellastub.Failure{Status: 409, Code: "phase_conflict"})
		again, err := Open(t.Context(), f.o)
		if err != nil {
			t.Fatal(err)
		}
		if rs, ok := again.Restarted(); !ok || !rs.OutOfMemory || again.Info().ID != id || f.stub.Count(cellastub.OpDelete) != 0 {
			t.Errorf("an open whose first start was refused: %s, restarted %+v %v, %d deletes", again.Info().ID, rs, ok, f.stub.Count(cellastub.OpDelete))
		}
	})
	t.Run("a call marks the machine lost and deletes nothing", func(t *testing.T) {
		f := open(t)
		id := f.m.Info().ID
		f.stub.SetFailed(id, "OOMKilled")
		err := execErr(t, f.m)
		if !errors.Is(err, machine.ErrLost) || Code(err) != machine.CodeLost || !strings.Contains(err.Error(), "OOMKilled") {
			t.Fatalf("a call on a failed sandbox the core does not start: %v", err)
		}
		if err := statErr(t, f.m, "."); Code(err) != machine.CodeLost {
			t.Errorf("the call after it: %v", err)
		}
		if sb, ok := f.stub.Sandbox(id); !ok || sb.Status.Phase != "Failed" || f.stub.Count(cellastub.OpDelete) != 0 {
			t.Errorf("the sandbox after the calls: %+v, there %v, %d deletes", sb.Status, ok, f.stub.Count(cellastub.OpDelete))
		}
	})
	t.Run("a start Cella refuses for another reason deletes nothing", func(t *testing.T) {
		f := open(t, func(s *cellastub.Server, _ *Options) { s.RestartFailed(true) })
		id := f.m.Info().ID
		f.stub.SetFailed(id, "Exited")
		f.stub.Fail(cellastub.OpStart, cellastub.Failure{Status: 422, Code: "quota_exceeded"})
		if _, err := Open(t.Context(), f.o); Code(err) != machine.CodeUnavailable {
			t.Errorf("an open whose start was refused: %v", err)
		}
		f.stub.Fail(cellastub.OpStart, cellastub.Failure{Status: 503, Code: "driver_unavailable"})
		if err := execErr(t, f.m); err == nil || Code(err) != "" {
			t.Errorf("a call whose start could not be answered: %v", err)
		}
		if sb, ok := f.stub.Sandbox(id); !ok || f.stub.Count(cellastub.OpDelete) != 0 {
			t.Errorf("the sandbox after the refusals: %+v, there %v, %d deletes", sb.Status, ok, f.stub.Count(cellastub.OpDelete))
		}
		// The next call starts it.
		if err := execErr(t, f.m); err != nil || f.m.Info().ID != id {
			t.Errorf("the call after the refusals: %v", err)
		}
	})
	t.Run("a sandbox gone when it is started is created again", func(t *testing.T) {
		f := open(t, func(s *cellastub.Server, _ *Options) { s.RestartFailed(true) })
		id := f.m.Info().ID
		f.stub.SetFailed(id, "Exited")
		f.stub.Fail(cellastub.OpStart, cellastub.Failure{Status: 404, Code: "not_found"})
		f.stub.Remove(id)
		// The read by name finds nothing once the sandbox is removed, so
		// the open creates one.
		again, err := Open(t.Context(), f.o)
		if err != nil || !again.Created() {
			t.Fatalf("an open of a sandbox that is gone: created %v, %v", again != nil && again.Created(), err)
		}
		if err := again.Release(t.Context(), true); err != nil {
			t.Error(err)
		}
	})
}

// TestASandboxThatFailsAgainDoesNotSpin: a sandbox that fails again right
// after Cella started it is started once by the call that found it failed.
// The call fails with words that say so, nothing is deleted and the
// machine is not lost; the next call starts it again (spec 009).
func TestASandboxThatFailsAgainDoesNotSpin(t *testing.T) {
	f := open(t, func(s *cellastub.Server, _ *Options) { s.RestartFailed(true) })
	id := f.m.Info().ID
	var told restarts
	f.m.OnRestart(told.hook)
	f.stub.SetFailed(id, "OOMKilled")
	f.stub.RefailAfterStart(2, "OOMKilled")
	for attempt := 1; attempt <= 2; attempt++ {
		starts := f.stub.Count(cellastub.OpStart)
		err := execErr(t, f.m)
		if err == nil || errors.Is(err, machine.ErrLost) || !strings.Contains(err.Error(), "failed again right after it was started") || !strings.Contains(err.Error(), "OOMKilled") {
			t.Fatalf("call %d on a sandbox that fails as it starts: %v", attempt, err)
		}
		if n := f.stub.Count(cellastub.OpStart) - starts; n != 1 {
			t.Fatalf("call %d sent %d starts, want one", attempt, n)
		}
	}
	if n := len(told.all()); n != 0 {
		t.Errorf("the hook heard of %d restarts of a sandbox that never ran", n)
	}
	// The same at an open, which answers it as a machine that cannot be
	// had now, and replaces nothing.
	f.stub.RefailAfterStart(1, "OOMKilled")
	if _, err := Open(t.Context(), f.o); err == nil || !strings.Contains(err.Error(), "failed again right after it was started") {
		t.Errorf("an open of it: %v", err)
	}
	if n := f.stub.Count(cellastub.OpDelete); n != 0 {
		t.Fatalf("%d deletes of a sandbox that failed again", n)
	}
	// Once it stays up, the next call runs.
	if err := execErr(t, f.m); err != nil || f.m.Info().ID != id {
		t.Fatalf("the call once the sandbox stays up: %v", err)
	}
	if got := told.all(); len(got) != 1 || !got[0].OutOfMemory {
		t.Errorf("the hook heard %+v", got)
	}

	// A sandbox that runs for the wait and stops before the call's second
	// try fails that call too, and the next one wakes it.
	f.stub.SetFailed(id, "Exited")
	stopped := false
	f.m.OnRestart(func(context.Context, machine.Restart) error {
		stopped = f.stub.SetFailed(id, "Exited")
		return nil
	})
	err := statErr(t, f.m, ".")
	if !stopped || err == nil || !strings.Contains(err.Error(), "stopped again right after it was started") || errors.Is(err, machine.ErrLost) {
		t.Fatalf("a call whose sandbox stopped again before it ran: %v", err)
	}
	f.m.OnRestart(nil)
	if err := statErr(t, f.m, "."); err != nil {
		t.Errorf("the call after it: %v", err)
	}
	if n := f.stub.Count(cellastub.OpDelete); n != 0 {
		t.Fatalf("%d deletes at the end", n)
	}
}

// TestACommandThatDiesWithItsSandbox: a command whose sandbox is killed
// under it ends with what it wrote and what became of the machine, in the
// same call. On a core that starts the failed sandbox it is restarted,
// for memory when Cella says so, with its files and its helper back and
// the hook told; on a core that does not, it is lost and nothing is
// deleted (spec 009).
func TestACommandThatDiesWithItsSandbox(t *testing.T) {
	for name, c := range map[string]struct {
		restart bool
		reason  string
		want    machine.Stop
	}{
		"out of memory, started again": {true, "OOMKilled", machine.Stop{OutOfMemory: true, Restarted: true}},
		"exited, started again":        {true, "Exited", machine.Stop{Restarted: true}},
		"a core that starts none":      {false, "OOMKilled", machine.Stop{OutOfMemory: true, Lost: true}},
	} {
		t.Run(name, func(t *testing.T) {
			f := open(t, func(s *cellastub.Server, _ *Options) { s.RestartFailed(c.restart) })
			id := f.m.Info().ID
			if err := f.m.WriteFile(t.Context(), "work.txt", strings.NewReader("work"), 0); err != nil {
				t.Fatal(err)
			}
			var told restarts
			f.m.OnRestart(told.hook)
			st, err := f.m.ExecStream(t.Context(), machine.ExecRequest{Command: "echo started; sleep 1; echo late"})
			if err != nil {
				t.Fatal(err)
			}
			readUntil(t, st, "started\n")
			f.loseHelper(t)
			if !f.stub.Kill(id, c.reason) {
				t.Fatal("the stub has no such sandbox")
			}
			rest, rerr := io.ReadAll(st)
			res, err := st.Wait()
			if err != nil || rerr != nil || len(rest) != 0 {
				t.Fatalf("a command killed with its sandbox: %q, %v, %v", rest, rerr, err)
			}
			if res.Stopped == nil || *res.Stopped != c.want || res.ExitCode != machine.ExitKilled || res.TimedOut || res.Canceled {
				t.Fatalf("its result %+v, stop %+v, want %+v", res, res.Stopped, c.want)
			}
			if n := f.stub.Count(cellastub.OpDelete); n != 0 {
				t.Fatalf("%d deletes", n)
			}
			if !c.restart {
				if err := execErr(t, f.m); Code(err) != machine.CodeLost || len(told.all()) != 0 {
					t.Errorf("the call after the loss: %v, hook %+v", err, told.all())
				}
				return
			}
			if got := told.all(); len(got) != 1 || got[0].OutOfMemory != c.want.OutOfMemory {
				t.Errorf("the hook heard %+v", got)
			}
			if got := content(t, f.m, "work.txt"); got != "work" || f.m.Info().ID != id {
				t.Errorf("the workspace after the restart = %q on %s", got, f.m.Info().ID)
			}
			out, err := f.m.Exec(t.Context(), machine.ExecRequest{Command: "echo again"})
			if err != nil || string(out.Output) != "again\n" || out.Stopped != nil {
				t.Errorf("the command run again: %+v %v", out, err)
			}
		})
	}
}

// TestACommandWhoseSandboxDoesNotComeBack: a sandbox that fails again as
// it is started under a dying command leaves the command's result with
// why it does not run, and neither lost nor deleted; a sandbox that is
// gone leaves it lost (spec 009).
func TestACommandWhoseSandboxDoesNotComeBack(t *testing.T) {
	f := open(t, func(s *cellastub.Server, _ *Options) { s.RestartFailed(true) })
	id := f.m.Info().ID
	f.stub.RefailAfterStart(1, "OOMKilled")
	st, err := f.m.ExecStream(t.Context(), machine.ExecRequest{Command: "echo started; sleep 1"})
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, st, "started\n")
	f.stub.Kill(id, "OOMKilled")
	res, err := st.Wait()
	if err != nil || res.Stopped == nil || res.Stopped.Restarted || res.Stopped.Lost || !res.Stopped.OutOfMemory ||
		res.Stopped.Err == nil || !strings.Contains(res.Stopped.Err.Error(), "failed again right after it was started") {
		t.Fatalf("a command whose sandbox failed again: %+v, stop %+v, %v", res, res.Stopped, err)
	}
	if err := execErr(t, f.m); err != nil || f.stub.Count(cellastub.OpDelete) != 0 {
		t.Fatalf("the next call: %v, %d deletes", err, f.stub.Count(cellastub.OpDelete))
	}

	st, err = f.m.ExecStream(t.Context(), machine.ExecRequest{Command: "echo started; sleep 1"})
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, st, "started\n")
	f.stub.Kill(id, "Exited")
	f.stub.Remove(id)
	res, err = st.Wait()
	if err != nil || res.Stopped == nil || !res.Stopped.Lost || res.Stopped.Restarted {
		t.Fatalf("a command whose sandbox is gone: %+v, stop %+v, %v", res, res.Stopped, err)
	}
	if err := execErr(t, f.m); Code(err) != machine.CodeLost {
		t.Errorf("the call after it: %v", err)
	}
}
