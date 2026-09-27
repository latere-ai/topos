// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"latere.ai/x/pkg/hostsandbox"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/host"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// HostSessionsDir is the directory under the data directory that holds
// each host session's working directory, named by its id, and beside
// it the session's spill and stage directories.
const HostSessionsDir = "host-sessions"

// stageEnvironment are the variables of the server's environment a
// command on its host inherits; the driver adds HOME and TERM, and the
// machine sets HOME to the session's working directory.
var stageEnvironment = []string{"PATH", "LANG", "TZ"}

// SandboxDriver is the srt driver of a server's host sessions. It finds
// srt and what srt needs on the server's PATH, denies the server's home
// directory whole, and hands each stage PATH, LANG and TZ from the
// server's environment and nothing else of it.
func SandboxDriver(getenv func(string) string) (*hostsandbox.Driver, error) {
	home := strings.TrimSpace(getenv("HOME"))
	if home == "" || filepath.Clean(home) == "/" {
		return nil, fmt.Errorf("the host sandbox denies the server's home directory to every command, and HOME is %q; run toposd as a user with a home directory of its own", home)
	}
	path := getenv("PATH")
	return hostsandbox.New(hostsandbox.Config{
		Home:        home,
		Alternative: "Or leave TOPOS_HOST_SESSIONS off and run hosted sessions on Cella with TOPOS_CELLA_URL.",
		Look:        func(name string) (string, error) { return lookPath(path, name) },
		Lookup: func(name string) (string, bool) {
			if !slices.Contains(stageEnvironment, name) {
				return "", false
			}
			v := getenv(name)
			return v, v != ""
		},
	}), nil
}

// executables maps the components the driver checks for to the program
// that provides each: Bubblewrap's is bwrap.
var executables = map[string]string{"bubblewrap": "bwrap"}

// lookPath finds an executable on path, the server's PATH, rather than
// on this process's own.
func lookPath(path, name string) (string, error) {
	name = cmp.Or(executables[name], name)
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: %w", name, exec.ErrNotFound)
}

// HostOptions configure the host sessions of a server (spec 009).
type HostOptions struct {
	// DataDir is TOPOS_DATA_DIR. Each session works in a directory of
	// its own under HostSessionsDir, and no command reads anything else
	// in the data directory.
	DataDir string
	// Denied are the paths the server's configuration names, which no
	// command reads: the Cella token file and the helper directory.
	Denied []string
	// Driver runs every command as a stage.
	Driver hostsandbox.Sandbox
}

// probeTimeout bounds the start-up probe's command.
var probeTimeout = 30 * time.Second

// NewHost checks that the host sandbox runs on this host and confines,
// and returns the Machines of host sessions. The driver's preflight must
// pass, and a probe command must run inside the sandbox and be denied a
// read of a file in the data directory: a program on PATH proves
// neither, since Bubblewrap in a container without user namespaces is
// present and fails at launch.
func NewHost(ctx context.Context, o HostOptions) (Machines, error) {
	if o.Driver == nil {
		return nil, errors.New("host sessions: no sandbox driver")
	}
	if err := o.Driver.Preflight(ctx); err != nil {
		return nil, fmt.Errorf("host sessions: %w", err)
	}
	dataDir, err := resolve(o.DataDir)
	if err != nil {
		return nil, fmt.Errorf("host sessions: the data directory: %w", err)
	}
	o.DataDir = dataDir
	var denied []string
	for _, p := range o.Denied {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("host sessions: %s: %w", p, err)
		}
		denied = append(denied, abs)
		// A file mounted through symlinks, as a Kubernetes secret is, is
		// denied at its target too.
		if real, err := filepath.EvalSymlinks(abs); err == nil && real != abs {
			denied = append(denied, real)
		}
	}
	o.Denied = denied
	if err := o.probe(ctx); err != nil {
		return nil, fmt.Errorf("host sessions: %w", err)
	}
	return o.machines, nil
}

// resolve makes a directory, absolute and with its symlinks evaluated,
// so the sandbox's denial names the path its roots are granted under.
func resolve(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("none is set")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// machines opens a host session's machine. An agent that names roots or
// read paths is refused: they are paths of the server's own host.
func (o HostOptions) machines(ctx context.Context, s session.Session, m v1.Machine) (machine.Machine, error) {
	if len(m.Roots) > 0 || len(m.ReadPaths) > 0 {
		return nil, setup(CodeMachineUnavailable, errors.New("the agent names machine.roots or machine.readPaths, paths of the server's own host, which a session on it never reaches"))
	}
	return o.open(s.ID, m.Egress)
}

// hostDirs are a host session's working, spill and stage directories.
func hostDirs(dataDir, id string) (work, spill, stages string, err error) {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return "", "", "", fmt.Errorf("%q is not a session id", id)
	}
	work = filepath.Join(dataDir, HostSessionsDir, id)
	return work, work + ".spill", work + ".stages", nil
}

// open opens the machine of the session id, creating its working
// directory empty the first time. Its roots are the working and spill
// directories; the data directory, the configured files and the
// server's home are denied to every command.
func (o HostOptions) open(id string, egress []string) (machine.Machine, error) {
	work, spill, stages, err := hostDirs(o.DataDir, id)
	if err != nil {
		return nil, setup(CodeMachineUnavailable, err)
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, fmt.Errorf("host sessions: create the session directory: %w", err)
	}
	h, err := host.Open(host.Options{
		Workdir: work, SpillDir: spill, ID: id, DataDir: o.DataDir,
		WorktreeDir: filepath.Join(work, ".worktrees"),
		Environ:     []string{},
		Sandbox: &host.Sandbox{
			Driver: o.Driver, StageDir: stages, Egress: egress,
			Denied: append([]string{o.DataDir}, o.Denied...),
		},
	})
	if err != nil {
		return nil, err
	}
	return &hostSession{Host: h, driver: o.Driver, dataDir: o.DataDir, id: id, stages: stages}, nil
}

// hostSession is the machine of a host session, which removes the
// session's directories when the session ends.
type hostSession struct {
	*host.Host
	driver  hostsandbox.Sandbox
	dataDir string
	id      string
	stages  string
}

// Release at the session's end also stops the background jobs of the
// machines earlier turns opened, each turn on a machine of its own,
// before the directories they run in are removed.
func (s *hostSession) Release(ctx context.Context, end bool) error {
	err := s.Host.Release(ctx, end)
	if !end {
		return err
	}
	return errors.Join(err, host.StopJobs(ctx, s.driver, s.stages), RemoveHostSession(s.dataDir, s.id))
}

// RemoveHostSession removes the directories of the host session id
// under the data directory: its working, spill and stage directories.
// A session that never ran on this host has none, which is not an
// error.
func RemoveHostSession(dataDir, id string) error {
	work, spill, stages, err := hostDirs(dataDir, id)
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range []string{work, spill, stages} {
		if err := os.RemoveAll(d); err != nil {
			errs = append(errs, fmt.Errorf("host sessions: remove %s: %w", d, err))
		}
	}
	return errors.Join(errs...)
}

// probe runs one command as a session would, in a directory of its own,
// that prints a marker, reads a file the probe wrote in the data
// directory, and prints the read's exit code. The sandbox runs when the
// marker arrives, and confines when neither the file's content nor a
// zero exit code does.
func (o HostOptions) probe(ctx context.Context) (err error) {
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	canaryDir, err := os.MkdirTemp(o.DataDir, ".sandbox-probe-*")
	if err != nil {
		return fmt.Errorf("create the probe's file: %w", err)
	}
	defer func() {
		if rerr := os.RemoveAll(canaryDir); rerr != nil {
			err = errors.Join(err, fmt.Errorf("remove the probe's file: %w", rerr))
		}
	}()
	canary, content := filepath.Join(canaryDir, "canary"), hex.EncodeToString(secret)
	if err := os.WriteFile(canary, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write the probe's file: %w", err)
	}
	m, err := o.open("probe"+strings.TrimPrefix(filepath.Base(canaryDir), ".sandbox-probe"), nil)
	if err != nil {
		return fmt.Errorf("open the probe's machine: %w", err)
	}
	const alive = "topos-sandbox-probe-ran"
	res, xerr := m.Exec(ctx, machine.ExecRequest{Command: "echo " + alive + "; cat " + hostsandbox.ShellQuote(canary) + "; echo code=$?", Timeout: probeTimeout})
	if err := m.Release(context.WithoutCancel(ctx), true); err != nil {
		return errors.Join(xerr, fmt.Errorf("release the probe's machine: %w", err))
	}
	out := string(res.Output)
	switch {
	case xerr != nil:
		return fmt.Errorf("the host sandbox could not run a probe command: %w", xerr)
	case res.TimedOut || !strings.Contains(out, alive):
		return fmt.Errorf("the host sandbox could not run a probe command, which answered %q", truncate(out))
	case strings.Contains(out, content) || strings.Contains(out, "code=0"):
		return fmt.Errorf("the host sandbox let a probe command read %s in the data directory, so it does not confine commands on this host", canary)
	}
	return nil
}

// truncate keeps the start of a probe's answer for a start-up message.
func truncate(s string) string {
	const limit = 500
	if len(s) > limit {
		return s[:limit] + "..."
	}
	return s
}

// ByKind opens each session's machine by the kind it asks for: a host
// machine through host, which is nil when TOPOS_HOST_SESSIONS is off,
// and every other kind through cella.
func ByKind(cella, host Machines) Machines {
	return func(ctx context.Context, s session.Session, m v1.Machine) (machine.Machine, error) {
		if cmp.Or(s.Machine.Kind, m.Kind) != session.MachineHost {
			return cella(ctx, s, m)
		}
		if host == nil {
			return nil, setup(CodeMachineUnavailable, errors.New("TOPOS_HOST_SESSIONS is off, so this server runs no session on its own host"))
		}
		return host(ctx, s, m)
	}
}
