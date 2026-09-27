// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"latere.ai/x/pkg/hostsandbox"

	"latere.ai/x/topos/machine"
)

// stagedSandbox is the fixture's host sandbox: the srt driver over
// shimDriver, with its stage directory and the data directory denied
// under base.
func stagedSandbox(t *testing.T, home, base string) *Sandbox {
	return &Sandbox{Driver: shimDriver(t, home), StageDir: filepath.Join(base, "stages"), Denied: []string{filepath.Join(base, "data")}}
}

// shimDriver is the srt driver over a stand-in for srt that drops
// `--settings <file> --` and runs the command, with every other program
// the driver checks for present. Its stages inherit PATH alone.
func shimDriver(t *testing.T, home string) *hostsandbox.Driver {
	t.Helper()
	srt := filepath.Join(t.TempDir(), "srt")
	if err := os.WriteFile(srt, []byte("#!/bin/sh\nshift 3\nexec \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return hostsandbox.New(hostsandbox.Config{
		Home: home, StopGrace: time.Minute,
		Look: func(name string) (string, error) {
			if name == "srt" {
				return srt, nil
			}
			return Shell, nil
		},
		Lookup: func(name string) (string, bool) {
			if name == "PATH" {
				return os.Getenv("PATH"), true
			}
			return "", false
		},
	})
}

// recorder is a driver that keeps the specs it launched and fails the
// call it is told to.
type recorder struct {
	hostsandbox.Sandbox
	mu    sync.Mutex
	specs []hostsandbox.StageSpec
	fail  string
}

var errInjected = errors.New("injected")

func (r *recorder) failing(call string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail == call {
		return errInjected
	}
	return nil
}

func (r *recorder) Launch(ctx context.Context, s hostsandbox.StageSpec) (hostsandbox.StageHandle, error) {
	r.mu.Lock()
	r.specs = append(r.specs, s)
	r.mu.Unlock()
	if err := r.failing("launch"); err != nil {
		return hostsandbox.StageHandle{}, err
	}
	return r.Sandbox.Launch(ctx, s)
}

func (r *recorder) Output(ctx context.Context, h hostsandbox.StageHandle, offset int64) (io.ReadCloser, error) {
	if err := r.failing("output"); err != nil {
		return nil, err
	}
	return r.Sandbox.Output(ctx, h, offset)
}

func (r *recorder) Observe(ctx context.Context, h hostsandbox.StageHandle) (hostsandbox.StageStatus, error) {
	if err := r.failing("observe"); err != nil {
		return hostsandbox.StageStatus{}, err
	}
	return r.Sandbox.Observe(ctx, h)
}

func (r *recorder) Discard(ctx context.Context, h hostsandbox.StageHandle) error {
	if err := r.failing("discard"); err != nil {
		return err
	}
	return r.Sandbox.Discard(ctx, h)
}

func (r *recorder) last() hostsandbox.StageSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.specs[len(r.specs)-1]
}

// staging opens a sandboxed host over a recorder of the shim driver.
func staging(t *testing.T, egress ...string) (*Host, *recorder, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(base, "data", "host-sessions", "ses_1")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{Sandbox: shimDriver(t, filepath.Join(base, "home"))}
	h, err := Open(Options{
		Workdir: work, SpillDir: work + ".spill", ID: "ses_1",
		Environ: []string{"PATH=" + os.Getenv("PATH"), "TOPOS_CREDENTIALS_KEY=k", "LANG=C"},
		Sandbox: &Sandbox{Driver: rec, StageDir: work + ".stages", Denied: []string{filepath.Join(base, "data"), "/etc/topos/token"}, Egress: egress},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Release(context.Background(), true); err != nil {
			t.Error(err)
		}
	})
	return h, rec, base
}

// TestAStageRunsWithTheSessionsPolicy: each command is a stage whose
// roots are readable and writable, whose denied paths are the
// sandbox's, whose log lies outside every root, whose network is the
// egress, and whose environment is HOME, the private temporary
// directory and the request's variables, none of the machine's own.
func TestAStageRunsWithTheSessionsPolicy(t *testing.T) {
	h, rec, base := staging(t, "api.example.com")
	if h.Info().Sandbox != string(hostsandbox.Host) {
		t.Fatalf("info %+v", h.Info())
	}
	res, err := h.Exec(t.Context(), machine.ExecRequest{Command: "env", Env: map[string]string{"EXTRA": "added"}})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("env: %+v, %v", res, err)
	}
	vars := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(res.Output)), "\n") {
		k, v, _ := strings.Cut(line, "=")
		vars[k] = v
	}
	work := h.Info().Workdir
	if vars["HOME"] != work || vars["TERM"] != "dumb" || vars["EXTRA"] != "added" || vars[srtTempDir] != filepath.Join(h.SpillDir(), "tmp") {
		t.Fatalf("the stage's environment %v", vars)
	}
	for _, name := range []string{"TOPOS_CREDENTIALS_KEY", "LANG"} {
		if _, ok := vars[name]; ok {
			t.Fatalf("the machine's environment reached the stage: %s in %v", name, vars)
		}
	}
	spec := rec.last()
	var paths []string
	for _, p := range spec.Paths {
		if p.Access != hostsandbox.ReadWrite {
			t.Fatalf("a root is not writable: %+v", p)
		}
		paths = append(paths, p.Host)
	}
	if !slices.Equal(paths, h.Roots()) || !slices.Equal(spec.Denied, []string{filepath.Join(base, "data"), "/etc/topos/token"}) ||
		spec.Network.Mode != hostsandbox.NetworkAllowlist || !slices.Equal(spec.Network.Domains, []string{"api.example.com"}) ||
		!strings.HasPrefix(spec.LogPath, work+".stages"+string(filepath.Separator)) || spec.Workdir != work {
		t.Fatalf("the stage %+v", spec)
	}
	for _, r := range h.Roots() {
		if strings.HasPrefix(spec.LogPath, r+string(filepath.Separator)) {
			t.Fatalf("the stage log %s is inside the root %s", spec.LogPath, r)
		}
	}
	if left, err := os.ReadDir(work + ".stages"); err != nil || len(left) != 0 {
		t.Fatalf("stage directories left behind: %v, %v", left, err)
	}
	none, rec, _ := staging(t)
	if _, err := none.Exec(t.Context(), machine.ExecRequest{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	if n := rec.last().Network; n.Mode != hostsandbox.NetworkNone || len(n.Domains) != 0 {
		t.Fatalf("no egress is no network: %+v", n)
	}
}

// TestAStrayChildEndsWithItsCommand: a child a command leaves behind is
// killed once the command has exited, as on the host.
func TestAStrayChildEndsWithItsCommand(t *testing.T) {
	h, _, _ := staging(t)
	pidFile := filepath.Join(h.SpillDir(), "stray.pid")
	res, err := h.Exec(t.Context(), machine.ExecRequest{Command: "/bin/sleep 30 & echo $! > " + pidFile})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("%+v, %v", res, err)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Sscan(string(b), &pid); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); processAlive(pid); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the stray child %d outlived its command", pid)
		}
	}
}

// processAlive reports whether a process runs. A killed child whose
// parent has exited is reaped by init at once, so a signal stops
// reaching it.
func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// TestOpenRefusesABadSandbox: a sandbox needs a driver and a stage
// directory outside every root.
func TestOpenRefusesABadSandbox(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	driver := shimDriver(t, dir)
	for name, sb := range map[string]*Sandbox{
		"no driver":            {StageDir: filepath.Join(dir, "stages")},
		"no stage directory":   {Driver: driver},
		"stages in the root":   {Driver: driver, StageDir: filepath.Join(dir, "w", "stages")},
		"stages over the root": {Driver: driver, StageDir: dir},
		"stages under a file":  {Driver: driver, StageDir: filepath.Join(dir, "file", "stages")},
	} {
		write(t, filepath.Join(dir, "file"), "x")
		if err := os.MkdirAll(filepath.Join(dir, "w"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(Options{Workdir: filepath.Join(dir, "w"), SpillDir: filepath.Join(dir, "s"), Sandbox: sb}); err == nil {
			t.Fatalf("%s: opened", name)
		}
	}
}

// TestAStageTheDriverFails: every driver failure is the command's error,
// and a failed start leaves no stage directory behind.
func TestAStageTheDriverFails(t *testing.T) {
	for _, call := range []string{"launch", "output", "observe", "discard"} {
		t.Run(call, func(t *testing.T) {
			h, rec, _ := staging(t)
			rec.fail = call
			for _, r := range []machine.ExecRequest{{Command: "echo x"}, {Command: "echo x", Background: true}} {
				res, err := h.Exec(t.Context(), r)
				if r.Background && err == nil && (call == "observe" || call == "discard") {
					// A job's observation and discard fail after it
					// started, and the session's end reports them. The
					// job ends with the failure still injected, so the
					// report is certain rather than a race with the
					// failure's removal below.
					waitForJobEnd(t, res.Log)
					if err := h.Release(t.Context(), true); !errors.Is(err, errInjected) {
						t.Fatalf("the session's end reported %v, want the job's %s failure", err, call)
					}
					continue
				}
				if !errors.Is(err, errInjected) {
					t.Fatalf("%+v: %+v, %v", r, res, err)
				}
			}
			rec.mu.Lock()
			rec.fail = ""
			rec.mu.Unlock()
			if call == "launch" || call == "output" {
				if left, err := os.ReadDir(h.opts.Sandbox.StageDir); err != nil || len(left) != 0 {
					t.Fatalf("a failed start left %v, %v", left, err)
				}
			}
		})
	}
	h, _, _ := staging(t)
	if _, err := h.Exec(t.Context(), machine.ExecRequest{Command: "true", Dir: "absent"}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing start directory: %v", err)
	}
	if _, err := h.Exec(t.Context(), machine.ExecRequest{Command: "true", Stdin: io.MultiReader(strings.NewReader("a"), failing{})}); err == nil {
		t.Fatal("an input that failed to read started")
	}
	if err := h.Release(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ExecStream(t.Context(), machine.ExecRequest{Command: "true"}); !errors.Is(err, machine.ErrReleased) {
		t.Fatalf("a stream after release: %v", err)
	}
}

// waitForJobEnd waits until a background job's log carries its exit
// line and returns the log.
func waitForJobEnd(t *testing.T, log string) string {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		b, err := os.ReadFile(log)
		if err == nil && strings.Contains(string(b), "exited with code") {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job's log %q, %v", b, err)
		}
	}
}

// TestAJobTheDriverLosesIsReported: a job whose observation fails ends
// with its exit line, and the session's end reports the failure.
func TestAJobTheDriverLosesIsReported(t *testing.T) {
	h, rec, _ := staging(t)
	job, err := h.Exec(t.Context(), machine.ExecRequest{Command: "/bin/sleep 30", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	rec.fail = "observe"
	rec.mu.Unlock()
	if b := waitForJobEnd(t, job.Log); !strings.Contains(b, "exited with code -1") {
		t.Fatalf("the job's log %q", b)
	}
	if err := h.Release(t.Context(), true); !errors.Is(err, errInjected) {
		t.Fatalf("release: %v", err)
	}
}

func TestStagePID(t *testing.T) {
	for id, want := range map[string]int{"pid:42@1700000000:/l/out.log": 42, "pid:x@1:/l": 0, "pid:0@1:/l": 0, "pid:42": 0, "42@1:/l": 0} {
		if got := stagePID(hostsandbox.StageHandle{Driver: hostsandbox.Host, ID: id}); got != want {
			t.Errorf("%s: %d, want %d", id, got, want)
		}
	}
	if got := stagePID(hostsandbox.StageHandle{Driver: "container", ID: "pid:42@1:/l"}); got != 0 {
		t.Errorf("another driver's handle: %d", got)
	}
}

func TestEgressAllows(t *testing.T) {
	egress := []string{"api.example.com", "*.pkg.dev"}
	for host, want := range map[string]bool{
		"api.example.com": true, "API.example.com.": true, "example.com": false, "x.api.example.com": false,
		"go.pkg.dev": true, "a.b.pkg.dev": true, "pkg.dev": false, "evilpkg.dev": false,
	} {
		if got := egressAllows(egress, host); got != want {
			t.Errorf("%s: %v, want %v", host, got, want)
		}
	}
}

// TestASandboxedFetchReachesOnlyTheEgress: a sandboxed host fetches a
// URL whose host, and every redirect's, is one of its egress hosts, and
// refuses every other.
func TestASandboxedFetchReachesOnlyTheEgress(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "hello") })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	mux.HandleFunc("/away", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://localhost:"+u.Port()+"/page", http.StatusFound)
	})
	h, _, _ := staging(t, u.Hostname())
	res, err := h.Fetch(t.Context(), machine.FetchRequest{URL: srv.URL + "/page"})
	if err != nil || string(res.Body) != "hello" {
		t.Fatalf("an egress host: %+v, %v", res, err)
	}
	if _, err := h.Fetch(t.Context(), machine.FetchRequest{URL: srv.URL + "/away"}); err == nil || !strings.Contains(err.Error(), "localhost is not one of the hosts") {
		t.Fatalf("a redirect off the egress: %v", err)
	}
	none, _, _ := staging(t)
	if _, err := none.Fetch(t.Context(), machine.FetchRequest{URL: srv.URL + "/page"}); err == nil || !strings.Contains(err.Error(), "is not one of the hosts") {
		t.Fatalf("a machine with no egress: %v", err)
	}
}
