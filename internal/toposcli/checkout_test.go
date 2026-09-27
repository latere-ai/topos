// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package toposcli

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/topos/session"
)

// TestASecondRunInACheckoutGetsAWorktree: topos run in a git checkout
// writes it in place; a second run there while the first session has
// not ended writes a worktree of its own under the data directory, on
// agents/<agent>/<session>.
func TestASecondRunInACheckoutGetsAWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	f := setup(t)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=p", "-c", "user.email=p@example.com", "commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir, cmd.Env = f.work, append(os.Environ(), "HOME="+f.vars["HOME"])
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	f.stub.Script(model, reply(text("First.")), reply(text("Second.")))
	for range 2 {
		if code, _, errOut := f.run("run", "--model", model, "Say hello."); code != ExitOK {
			t.Fatalf("exit %d, stderr %q", code, errOut)
		}
	}
	list := f.sessions()
	if len(list) != 2 {
		t.Fatalf("%d sessions", len(list))
	}
	slices.SortFunc(list, func(a, b session.Session) int { return strings.Compare(a.ID, b.ID) })
	first, second := list[0], list[1]
	if first.Machine.Workdir != f.work {
		t.Fatalf("the first session writes %s", first.Machine.Workdir)
	}
	want := filepath.Join(f.vars["TOPOS_DATA_DIR"], "worktrees", second.ID)
	if second.Machine.Workdir != want {
		t.Fatalf("the second session writes %s, want %s", second.Machine.Workdir, want)
	}
	cmd := exec.CommandContext(t.Context(), "git", "branch", "--show-current")
	cmd.Dir = want
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "agents/topos/"+second.ID {
		t.Fatalf("the worktree is on %q, %v", out, err)
	}
}
