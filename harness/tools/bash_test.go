// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/host"
)

func TestBashOutputAndExitCode(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	bash := builtinTool(t, NameBash)
	res := run(ctx, t, bash, f.h, State{}, `{"command":"echo out; echo err >&2; echo again; exit 3","description":"print and fail"}`)
	if res.Outcome != OutcomeOK || res.IsError() || text(res) != "out\nerr\nagain\nExit code: 3" {
		t.Fatalf("exit code %s %q", res.Outcome, text(res))
	}
	if res.Meta == nil || res.Meta.ExitCode == nil || *res.Meta.ExitCode != 3 || res.Meta.Dir != f.work {
		t.Fatalf("meta %+v", res.Meta)
	}
	res = run(ctx, t, bash, f.h, State{}, `{"command":":"}`)
	if text(res) != "(no output)\nExit code: 0" {
		t.Fatalf("no output %q", text(res))
	}
	res = run(ctx, t, bash, f.h, State{}, `{"command":"printf abc"}`)
	if text(res) != "abc\nExit code: 0" {
		t.Fatalf("no final newline %q", text(res))
	}
	res = run(ctx, t, bash, f.h, State{}, `{"command":"   "}`)
	if res.Outcome != OutcomeError || text(res) != "The command is empty." {
		t.Fatalf("an empty command %s %q", res.Outcome, text(res))
	}
}

func TestBashPersistentDirectory(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	bash := builtinTool(t, NameBash)
	sub := filepath.Join(f.work, "sub")
	if err := os.MkdirAll(filepath.Join(sub, "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	th := &thread{id: "thr_main"}
	res := th.call(ctx, t, bash, f.h, `{"command":"cd sub && export KEPT=no"}`)
	if res.Meta.Dir != sub {
		t.Fatalf("the directory %q", res.Meta.Dir)
	}
	// A fresh harness reads the directory from the log.
	fresh := &thread{id: th.id, events: th.events}
	res = fresh.call(ctx, t, bash, f.h, `{"command":"pwd -P; echo ${KEPT:-unset}; cd deeper"}`)
	if text(res) != sub+"\nunset\nExit code: 0" || res.Meta.Dir != filepath.Join(sub, "deeper") {
		t.Fatalf("the next call %q in %q", text(res), res.Meta.Dir)
	}

	// The persistent directory is removed: the call runs in the working
	// directory and says so.
	res = fresh.call(ctx, t, bash, f.h, `{"command":"cd .."}`)
	if res.Meta.Dir != sub {
		t.Fatalf("back up %q", res.Meta.Dir)
	}
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	res = fresh.call(ctx, t, bash, f.h, `{"command":"pwd -P"}`)
	want := fmt.Sprintf("[%s no longer exists; the command ran in the working directory %s]\n%s\nExit code: 0", sub, f.work, f.work)
	if res.Outcome != OutcomeOK || text(res) != want || res.Meta.Dir != f.work {
		t.Fatalf("a removed directory %q in %q", text(res), res.Meta.Dir)
	}
	if fresh.state().Dir != f.work {
		t.Fatalf("the state after the fallback %q", fresh.state().Dir)
	}
	res = run(ctx, t, bash, f.h, State{Dir: filepath.Join(f.work, "gone")}, `{"command":"exit 7"}`)
	if !strings.HasPrefix(text(res), "[") || res.Meta.Dir != f.work || *res.Meta.ExitCode != 7 {
		t.Fatalf("an exit in the fallback %q, %+v", text(res), res.Meta)
	}
}

func TestBashTimeoutKillsGroup(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	bash := builtinTool(t, NameBash)
	th := &thread{id: "thr_main"}
	th.call(ctx, t, bash, f.h, `{"command":"cd /"}`)
	start := time.Now()
	pgid := filepath.Join(f.work, "pgid")
	res := th.call(ctx, t, bash, f.h, mustInput(t, map[string]any{"command": "echo $$ > " + pgid + "; echo started; /bin/sleep 30 & /bin/sleep 30", "timeout_ms": 200}))
	if res.Outcome != OutcomeTimeout || !res.IsError() || time.Since(start) > 10*time.Second {
		t.Fatalf("timeout %s after %s", res.Outcome, time.Since(start))
	}
	if text(res) != "started\nThe command passed its timeout of 200ms and was killed with its process group." {
		t.Fatalf("timeout text %q", text(res))
	}
	// The shell leads its own process group, so its pid names the group;
	// the background sleep in it must be gone too.
	group := strings.TrimSpace(get(t, pgid))
	deadline := time.Now().Add(5 * time.Second)
	for {
		probe := run(ctx, t, bash, f.h, State{}, mustInput(t, map[string]string{"command": "kill -0 -- -" + group}))
		if probe.Meta.ExitCode != nil && *probe.Meta.ExitCode != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process group %s survived the timeout: %q", group, text(probe))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if res.Meta.Dir != "" || th.state().Dir != "/" {
		t.Fatalf("a killed shell keeps the directory: %q, state %q", res.Meta.Dir, th.state().Dir)
	}
	// A timeout in the fallback still leaves the removed directory.
	res = run(ctx, t, bash, f.h, State{Dir: filepath.Join(f.work, "gone")}, `{"command":"/bin/sleep 30","timeout_ms":200}`)
	if res.Outcome != OutcomeTimeout || res.Meta.Dir != f.work {
		t.Fatalf("a timeout in the fallback %s in %q", res.Outcome, res.Meta.Dir)
	}
}

func TestBashCanceled(t *testing.T) {
	f := open(t)
	old := host.KillGrace
	host.KillGrace = 200 * time.Millisecond
	t.Cleanup(func() { host.KillGrace = old })
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	res := run(ctx, t, builtinTool(t, NameBash), f.h, State{}, `{"command":"/bin/sleep 30"}`)
	if res.Outcome != OutcomeCanceled || text(res) != "(no output)\nThe command was canceled and killed with its process group." {
		t.Fatalf("cancel %s %q", res.Outcome, text(res))
	}
}

var jobPattern = regexp.MustCompile(`^Started in the background as pid (\d+); its output goes to (\S+)\. Read the log with read, and stop the job with bash: kill -TERM -(\d+)\.$`)

func TestBashBackgroundJob(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	bash, read := builtinTool(t, NameBash), builtinTool(t, NameRead)
	start := time.Now()
	res := run(ctx, t, bash, f.h, State{}, `{"command":"echo serving; /bin/sleep 30","background":true}`)
	if res.Outcome != OutcomeOK || time.Since(start) > 5*time.Second || res.Meta != nil {
		t.Fatalf("background %s %q after %s", res.Outcome, text(res), time.Since(start))
	}
	m := jobPattern.FindStringSubmatch(text(res))
	if m == nil || m[1] != m[3] || !strings.HasPrefix(m[2], f.spill) {
		t.Fatalf("background text %q", text(res))
	}
	pid, err := strconv.Atoi(m[1])
	if err != nil || pid <= 0 {
		t.Fatalf("pid %q", m[1])
	}
	waitLog(ctx, t, read, f.h, m[2], "serving")
	stop := run(ctx, t, bash, f.h, State{}, fmt.Sprintf(`{"command":"kill -TERM -%d"}`, pid))
	if stop.Outcome != OutcomeOK || !strings.HasSuffix(text(stop), "Exit code: 0") {
		t.Fatalf("stop %q", text(stop))
	}
	waitLog(ctx, t, read, f.h, m[2], "exited with code")

	res = run(ctx, t, bash, f.h, State{Dir: filepath.Join(f.work, "gone")}, `{"command":"/bin/sleep 30","background":true}`)
	if !strings.HasPrefix(text(res), "["+filepath.Join(f.work, "gone")+" no longer exists") || !strings.Contains(text(res), "Started in the background") {
		t.Fatalf("a background job in a removed directory %q", text(res))
	}
}

// waitLog reads a job log with the read tool until it holds want.
func waitLog(ctx context.Context, t *testing.T, read Tool, m machine.Machine, log, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		res := run(ctx, t, read, m, State{}, mustInput(t, map[string]string{"path": log}))
		if strings.Contains(text(res), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the log never held %q: %q", want, text(res))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBashMachineFailures(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	bash := builtinTool(t, NameBash)
	res := run(ctx, t, bash, faulty{Machine: f.h, execErr: errors.New("no shell")}, State{}, `{"command":"echo"}`)
	if res.Outcome != OutcomeError || text(res) != "The command did not start: no shell." {
		t.Fatalf("a failed start %s %q", res.Outcome, text(res))
	}
	// A spill file that cannot be written is a failure of the harness.
	big := `{"command":"i=0; while [ $i -lt 400 ]; do printf '%099d\\n' 0; i=$((i+1)); done"}`
	if _, err := bash.Run(ctx, Call{ID: "toolu_1", Input: []byte(big), Machine: faulty{Machine: f.h, writeErr: errors.New("no space left")}}); err == nil || !strings.Contains(err.Error(), "no space left") {
		t.Fatalf("a failed spill: %v", err)
	}
	if err := f.h.Release(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := bash.Run(ctx, Call{Input: []byte(`{"command":"echo"}`), Machine: f.h}); !errors.Is(err, machine.ErrReleased) {
		t.Fatalf("a released machine %v", err)
	}
}

func TestOutputSpill(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	bash := builtinTool(t, NameBash)
	// 1024 lines of 99 digits and a newline: 100 KiB.
	cmd := `i=0; while [ $i -lt 1024 ]; do printf '%099d\n' $i; i=$((i+1)); done`
	res := run(ctx, t, bash, f.h, State{}, mustInput(t, map[string]string{"command": cmd}))
	if res.Outcome != OutcomeOK || res.Spill == nil {
		t.Fatalf("no spill: %s, %d bytes", res.Outcome, len(text(res)))
	}
	full := get(t, res.Spill.Path)
	if !strings.HasPrefix(res.Spill.Path, f.spill+"/") || strings.HasPrefix(res.Spill.Path, f.work) {
		t.Fatalf("the spill file %s is not in the spill directory", res.Spill.Path)
	}
	wantFull := 100<<10 + len("Exit code: 0")
	if len(full) != wantFull || res.Spill.Bytes != int64(wantFull) {
		t.Fatalf("the spill file holds %d bytes, recorded %d, want %d", len(full), res.Spill.Bytes, wantFull)
	}
	got := text(res)
	line := fmt.Sprintf("\n[... %d bytes omitted; the full output is in %s ...]\n", wantFull-30<<10, res.Spill.Path)
	if got != full[:20<<10]+line+full[len(full)-10<<10:] {
		t.Fatalf("the capped text is not 20 KiB of head, the line and 10 KiB of tail: %d bytes", len(got))
	}
}
