// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"latere.ai/x/pkg/hostsandbox"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/host"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/srtstub"
)

// system is the PATH of the programs a stage's shell runs.
const system = "/usr/bin:/bin"

// serverEnv is a server's environment: its PATH, a home, and variables
// no command may see.
func serverEnv(t *testing.T, path string) func(string) string {
	t.Helper()
	vars := map[string]string{
		"PATH": path, "HOME": t.TempDir(), "LANG": "C", "USER": "topos", "SHELL": "/bin/sh",
		"TOPOS_CREDENTIALS_KEY": "server-secret", "DATABASE_URL": "postgres://server-secret@db/topos",
	}
	return func(k string) string { return vars[k] }
}

func TestSandboxDriver(t *testing.T) {
	for _, home := range []string{"", " ", "/"} {
		if _, err := SandboxDriver(func(k string) string { return map[string]string{"HOME": home}[k] }); err == nil || !strings.Contains(err.Error(), "HOME") {
			t.Fatalf("HOME %q: %v", home, err)
		}
	}
	d, err := SandboxDriver(serverEnv(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	var nr *hostsandbox.NotReadyError
	if err := d.Preflight(t.Context()); !errors.As(err, &nr) || !strings.Contains(err.Error(), "srt") || !strings.Contains(err.Error(), "TOPOS_HOST_SESSIONS off") {
		t.Fatalf("no srt on the server's PATH: %v", err)
	}
	if d, err = SandboxDriver(serverEnv(t, srtstub.Bin(t, srtstub.Unconfined)+":"+system)); err != nil {
		t.Fatal(err)
	}
	if err := d.Preflight(t.Context()); err != nil {
		t.Fatalf("srt and what it needs on the server's PATH: %v", err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "plain"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(bin, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"plain", "dir", "absent"} {
		if _, err := lookPath("::"+bin, name); !errors.Is(err, exec.ErrNotFound) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if p, err := lookPath(srtstub.Bin(t, srtstub.Unconfined), "bubblewrap"); err != nil || filepath.Base(p) != "bwrap" {
		t.Fatalf("bubblewrap is bwrap: %s, %v", p, err)
	}
}

// TestNewHostRefusesASandboxThatDoesNotHold: a server does not start its
// host sessions without a driver, without srt, with a sandbox that
// cannot run a command, or with one that lets a command read the data
// directory.
func TestNewHostRefusesASandboxThatDoesNotHold(t *testing.T) {
	if _, err := NewHost(t.Context(), HostOptions{DataDir: t.TempDir()}); err == nil {
		t.Fatal("host sessions without a driver")
	}
	for name, c := range map[string]struct{ path, want string }{
		"no srt":       {t.TempDir(), "srt: missing"},
		"a broken srt": {srtstub.Bin(t, srtstub.Broken) + ":" + system, "could not run a probe command"},
		"no confining": {srtstub.Bin(t, srtstub.Unconfined) + ":" + system, "does not confine commands on this host"},
		"no data dir":  {srtstub.Bin(t, srtstub.Unconfined) + ":" + system, "the data directory"},
	} {
		d, err := SandboxDriver(serverEnv(t, c.path))
		if err != nil {
			t.Fatal(err)
		}
		o := HostOptions{DataDir: t.TempDir(), Driver: d, Denied: []string{"relative/token", filepath.Join(t.TempDir(), "absent")}}
		if name == "no data dir" {
			o.DataDir = ""
		}
		_, err = NewHost(t.Context(), o)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v", name, err)
		}
		if o.DataDir != "" {
			left, rerr := filepath.Glob(filepath.Join(o.DataDir, ".sandbox-probe-*"))
			if rerr != nil || len(left) != 0 {
				t.Fatalf("%s: the probe left %v, %v", name, left, rerr)
			}
			if left, rerr := os.ReadDir(filepath.Join(o.DataDir, HostSessionsDir)); rerr == nil && len(left) != 0 {
				t.Fatalf("%s: the probe's machine left %v", name, left)
			}
		}
	}
}

// failingLaunch is a driver whose every launch fails.
type failingLaunch struct{ hostsandbox.Sandbox }

func (failingLaunch) Launch(context.Context, hostsandbox.StageSpec) (hostsandbox.StageHandle, error) {
	return hostsandbox.StageHandle{}, errors.New("no namespaces")
}

// TestAProbeThatCannotRun: a probe command the driver cannot start, and
// one whose sandbox answers at length, each refuse the start with what
// they answered, cut short.
func TestAProbeThatCannotRun(t *testing.T) {
	d, err := SandboxDriver(serverEnv(t, srtstub.Bin(t, srtstub.Unconfined)+":"+system))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewHost(t.Context(), HostOptions{DataDir: t.TempDir(), Driver: failingLaunch{d}}); err == nil || !strings.Contains(err.Error(), "could not run a probe command: machine: start the command: no namespaces") {
		t.Fatalf("a launch that fails: %v", err)
	}
	loud := "#!/bin/sh\nprintf '%0600d' 0\nexit 1\n"
	if d, err = SandboxDriver(serverEnv(t, srtstub.Bin(t, loud)+":"+system)); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHost(t.Context(), HostOptions{DataDir: t.TempDir(), Driver: d}); err == nil || !strings.Contains(err.Error(), strings.Repeat("0", 500)+"...") || strings.Contains(err.Error(), strings.Repeat("0", 501)) {
		t.Fatalf("a long answer: %v", err)
	}
	if truncate("short") != "short" {
		t.Fatal("a short answer was cut")
	}
}

// hostOptions are host sessions over the unconfined stand-in, for what
// the machines do around the sandbox rather than the sandbox itself.
func hostOptions(t *testing.T) HostOptions {
	t.Helper()
	d, err := SandboxDriver(serverEnv(t, srtstub.Bin(t, srtstub.Unconfined)+":"+system))
	if err != nil {
		t.Fatal(err)
	}
	data, err := resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return HostOptions{DataDir: data, Driver: d, Denied: []string{"/run/topos/cella-token"}}
}

// TestAHostSessionsMachine: a host session works in its own empty
// directory under the data directory, with the data directory and the
// configured files denied and the agent's egress, records the sandbox
// driver, removes its directories when it ends, and is refused when its
// agent names roots or read paths.
func TestAHostSessionsMachine(t *testing.T) {
	o := hostOptions(t)
	s := session.Session{ID: session.NewID(session.PrefixSession)}
	m, err := o.machines(t.Context(), s, v1.Machine{Kind: v1.MachineHost, Egress: []string{"api.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(o.DataDir, HostSessionsDir, s.ID)
	info := m.Info()
	if info.Kind != machine.KindHost || info.Workdir != work || info.Sandbox != string(hostsandbox.Host) || info.ID != s.ID ||
		m.SpillDir() != work+".spill" {
		t.Fatalf("info %+v spill %s", info, m.SpillDir())
	}
	if entries, err := os.ReadDir(work); err != nil || len(entries) != 0 {
		t.Fatalf("the session directory is not empty: %v, %v", entries, err)
	}
	res, err := m.Exec(t.Context(), machine.ExecRequest{Command: "pwd; echo $HOME; echo ${TOPOS_CREDENTIALS_KEY:-unset} ${USER:-unset} ${LANG:-unset}", Env: map[string]string{"GITHUB_TOKEN": "placeholder"}})
	if err != nil || string(res.Output) != work+"\n"+work+"\nunset unset C\n" {
		t.Fatalf("a command %q, %v", res.Output, err)
	}
	if _, ok := m.(machine.Fetcher); !ok {
		t.Fatal("a host session cannot fetch")
	}
	if _, ok := m.(machine.Worktrees); !ok {
		t.Fatal("a host session keeps no worktrees")
	}
	if err := m.Release(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	again, err := o.open(s.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "kept.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := again.Release(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{work, work + ".spill", work + ".stages"} {
		if _, err := os.Stat(d); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s outlived the session: %v", d, err)
		}
	}
	for _, m := range []v1.Machine{{Kind: v1.MachineHost, Roots: []string{"/srv"}}, {Kind: v1.MachineHost, ReadPaths: []string{"/etc"}}} {
		if _, err := o.machines(t.Context(), s, m); code(t, err) != CodeMachineUnavailable {
			t.Fatalf("%+v: %v", m, err)
		}
	}
	if _, err := o.open("../escape", nil); code(t, err) != CodeMachineUnavailable {
		t.Fatalf("a session id that leaves the directory: %v", err)
	}
	if err := RemoveHostSession(o.DataDir, ""); err == nil {
		t.Fatal("removed the directory of no session")
	}
	if err := RemoveHostSession(o.DataDir, session.NewID(session.PrefixSession)); err != nil {
		t.Fatalf("a session that never ran here: %v", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (HostOptions{DataDir: file, Driver: o.Driver}).open(s.ID, nil); err == nil {
		t.Fatal("opened a session under a file")
	}
	if _, err := resolve(filepath.Join(file, "x")); err == nil {
		t.Fatal("resolved a directory under a file")
	}
}

func TestByKind(t *testing.T) {
	opened := ""
	open := func(kind string) Machines {
		return func(context.Context, session.Session, v1.Machine) (machine.Machine, error) {
			opened = kind
			return nil, nil
		}
	}
	for _, c := range []struct {
		session, agent, want string
	}{{"", v1.MachineCella, "cella"}, {session.MachineCella, v1.MachineHost, "cella"}, {session.MachineHost, v1.MachineCella, "host"}, {"", v1.MachineHost, "host"}} {
		opened = ""
		if _, err := ByKind(open("cella"), open("host"))(t.Context(), session.Session{Machine: session.Machine{Kind: c.session}}, v1.Machine{Kind: c.agent}); err != nil || opened != c.want {
			t.Fatalf("%+v: opened %q, %v", c, opened, err)
		}
	}
	if _, err := ByKind(open("cella"), nil)(t.Context(), session.Session{}, v1.Machine{Kind: v1.MachineHost}); code(t, err) != CodeMachineUnavailable {
		t.Fatalf("host sessions off: %v", err)
	}
}

// TestHostSessionsAreConfined is the sandbox itself, with srt: a
// server's host sessions start past the probe, and a session's command
// reads its own directory and none of another session's, nothing else
// in the data directory, no configured file, and none of the server's
// environment.
func TestHostSessionsAreConfined(t *testing.T) {
	srt, err := exec.LookPath("srt")
	if err != nil {
		t.Skip("srt, the host sandbox's runtime, is not on PATH; the confinement of host sessions needs it")
	}
	vars := map[string]string{
		"PATH": filepath.Dir(srt) + ":" + os.Getenv("PATH"), "HOME": os.Getenv("HOME"), "LANG": "C",
		"TOPOS_CREDENTIALS_KEY": "server-secret", "USER": "topos",
	}
	d, err := SandboxDriver(func(k string) string { return vars[k] })
	if err != nil {
		t.Fatal(err)
	}
	data, err := resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(t.TempDir(), "cella-token")
	for path, body := range map[string]string{token: "cella-secret", filepath.Join(data, "sessions", "ses_x", "events.jsonl"): "store-secret"} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	machines, err := NewHost(t.Context(), HostOptions{DataDir: data, Driver: d, Denied: []string{token}})
	if err != nil {
		t.Fatalf("the probe refused srt: %v", err)
	}
	open := func() machine.Machine {
		m, err := machines(t.Context(), session.Session{ID: session.NewID(session.PrefixSession)}, v1.Machine{Kind: v1.MachineHost})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := m.Release(context.Background(), true); err != nil {
				t.Error(err)
			}
		})
		return m
	}
	a, b := open(), open()
	if res, err := a.Exec(t.Context(), machine.ExecRequest{Command: "echo mine > own.txt && cat own.txt"}); err != nil || string(res.Output) != "mine\n" {
		t.Fatalf("a session's own directory: %q, %v", res.Output, err)
	}
	for _, p := range []string{filepath.Join(a.Info().Workdir, "own.txt"), filepath.Join(data, "sessions", "ses_x", "events.jsonl"), token} {
		res, err := b.Exec(t.Context(), machine.ExecRequest{Command: "cat " + hostsandbox.ShellQuote(p)})
		if err != nil || res.ExitCode == 0 || strings.Contains(string(res.Output), "secret") || strings.Contains(string(res.Output), "mine") {
			t.Fatalf("another session read %s: %+v, %v", p, res, err)
		}
	}
	res, err := b.Exec(t.Context(), machine.ExecRequest{Command: "env"})
	if err != nil || strings.Contains(string(res.Output), "server-secret") || strings.Contains(string(res.Output), "USER=") || !strings.Contains(string(res.Output), "HOME="+b.Info().Workdir) {
		t.Fatalf("the server's environment reached a command: %s, %v", res.Output, err)
	}
}

// TestTheProbesOwnFailures: a probe that cannot write its file or open
// its machine refuses the start, and a session's directories that
// cannot be removed are an error.
func TestTheProbesOwnFailures(t *testing.T) {
	o := hostOptions(t)
	absent := o
	absent.DataDir = filepath.Join(t.TempDir(), "absent")
	if err := absent.probe(t.Context()); err == nil || !strings.Contains(err.Error(), "create the probe's file") {
		t.Fatalf("no data directory: %v", err)
	}
	blocked := o
	blocked.DataDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(blocked.DataDir, HostSessionsDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := blocked.probe(t.Context()); err == nil || !strings.Contains(err.Error(), "open the probe's machine") {
		t.Fatalf("no room for the probe's machine: %v", err)
	}
	spill := o
	id := session.NewID(session.PrefixSession)
	work, spillDir, _, err := hostDirs(spill.DataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(spillDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spillDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := spill.open(id, nil); err == nil {
		t.Fatal("opened a session whose spill directory is a file")
	}
	if err := os.MkdirAll(filepath.Join(work, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(work, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(work, 0o700); err != nil {
			t.Error(err)
		}
	})
	if err := RemoveHostSession(spill.DataDir, id); err == nil {
		t.Fatal("removed a session directory that cannot be removed")
	}
}

// TestAJobOfAnEarlierTurnEndsWithTheSession: a background job started on
// the machine of one drive, left running when that drive's machine was
// let go idle, is stopped when a later drive's machine ends the session.
func TestAJobOfAnEarlierTurnEndsWithTheSession(t *testing.T) {
	o := hostOptions(t)
	id := session.NewID(session.PrefixSession)
	first, err := o.open(id, nil)
	if err != nil {
		t.Fatal(err)
	}
	job, err := first.Exec(t.Context(), machine.ExecRequest{Command: "/bin/sleep 30", Background: true})
	if err != nil || job.PID == 0 {
		t.Fatalf("job %+v, %v", job, err)
	}
	if err := first.Release(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	last, err := o.open(id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := last.Release(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(-job.PID, 0); err == nil {
		t.Fatalf("the job's process group %d outlived its session", job.PID)
	}
	_, _, stages, err := hostDirs(o.DataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stages, "cmd-x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stages, "cmd-x", "handle.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := host.StopJobs(t.Context(), o.Driver, stages); err == nil {
		t.Fatal("stopped a job whose handle does not read")
	}
	if err := host.StopJobs(t.Context(), o.Driver, "["); err == nil {
		t.Fatal("stopped the jobs of a malformed directory")
	}
}
