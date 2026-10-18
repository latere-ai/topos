// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path"
	"strings"
	"time"

	"latere.ai/x/cella/client"
	v1 "latere.ai/x/cella/manifest/v1"

	"latere.ai/x/topos/machine"
)

// DefaultApprovalWait is how long an open waits for a computer's owner to
// allow the session before the turn stops to wait (spec 080). The create
// goes on waiting on the computer, and the next open finds it.
const DefaultApprovalWait = 2 * time.Minute

// DeviceDir is the directory under a device sandbox's temporary
// directory that holds the helper (bin/), the spill directory (spill/)
// and the files a person attached (attachments/): the computer's own
// sandbox lets a session write in its granted folder and its scratch
// directories alone, and the session's own files never go into the
// person's folder.
const DeviceDir = "topos"

// codeDriverUnavailable is Cella's answer for an environment whose worker
// does not answer: on a device, the computer is asleep or offline.
const codeDriverUnavailable = "driver_unavailable"

// The labels a device's sandbox carries beside the session's and the
// agent's (spec 080): the agent's version, and for a session a trigger
// started, the trigger and what it fires on. A computer shows them when
// it asks its owner to allow the session, and remembers a scheduled
// trigger by them. A sandbox with no trigger label was started by a
// person.
const (
	LabelAgentVersion   = "topos.latere.ai/agent-version"
	LabelTrigger        = "topos.latere.ai/trigger"
	LabelTriggerFiresOn = "topos.latere.ai/trigger-fires-on"
	// AnnotationAgentName is the agent's display name, which a label
	// cannot hold.
	AnnotationAgentName = "topos.latere.ai/agent-name"
)

// attachDevice finds a device's sandbox by name or creates it on the
// environment the computer serves, waits while the computer holds it for
// its owner, and puts the helper in its scratch directory. A computer
// that does not answer is machine.Waiting offline, one whose owner has
// not answered within ApprovalWait machine.Waiting approval, and one
// whose owner refused the session machine.Refused, after which the
// refused sandbox is deleted so the next open asks again.
func (m *Machine) attachDevice(ctx context.Context) error {
	if err := m.readHost(ctx); err != nil {
		return err
	}
	sb, raw, err := m.c.GetSandbox(ctx, m.name)
	created := false
	switch {
	case client.CodeOf(err) == "not_found":
		sb, raw, created, err = m.createDevice(ctx)
	case err != nil:
		err = m.offline(refused("find the sandbox "+m.name, err))
	}
	if err != nil {
		return err
	}
	sb, raw, restart, err := m.deviceRunning(ctx, sb, raw)
	if err != nil {
		return err
	}
	tmp, err := scratchOf(raw)
	if err != nil {
		return fmt.Errorf("%w: the sandbox %s: %w", ErrUnavailable, m.name, err)
	}
	m.mu.Lock()
	m.scratch = path.Join(tmp, DeviceDir)
	m.mu.Unlock()
	m.set(sb, created, restart)
	return m.ensureDeviceHelper(ctx, sb.Status.ID)
}

// deviceRunning holds until a device's sandbox runs, and answers it with
// the bytes of its last read. While the computer holds it for its owner,
// the first such read tells the session through machine.TellWaiting, so
// a client shows the person to allow it on the computer, and after
// ApprovalWait it answers machine.Waiting approval with the create left
// held. A sandbox the owner refused, or did not answer in time, is
// deleted and answered machine.Refused with the computer's own words. A
// stopped or failed one is started as a sandbox is; one the computer will
// not start again is deleted, since it holds the session's scratch files
// alone, and the next open makes a new one on the same computer and in
// the same folder.
func (m *Machine) deviceRunning(ctx context.Context, sb v1.Sandbox, raw []byte) (v1.Sandbox, []byte, *machine.Restart, error) {
	deadline := time.Now().Add(m.o.StartTimeout)
	var asked time.Time
	for {
		switch {
		case sb.Status.Phase == "Failed" && (sb.Status.Reason == v1.ReasonHostRefused || sb.Status.Reason == v1.ReasonHostTimeout):
			c, _ := hostApproval(sb)
			r := &machine.Refused{Environment: m.o.Environment, Detail: cmpOr(c.Message, sb.Status.Reason), Timeout: sb.Status.Reason == v1.ReasonHostTimeout}
			if _, err := m.c.Delete(ctx, client.KindSandbox, sb.Status.ID); err != nil && client.CodeOf(err) != "not_found" {
				return sb, raw, nil, errors.Join(r, refused("delete the refused sandbox "+m.name, err))
			}
			return sb, raw, nil, r
		case sb.Status.Phase == "Running":
			return sb, raw, nil, nil
		case heldBy(sb):
			if asked.IsZero() {
				asked = time.Now()
				if err := machine.TellWaiting(ctx, machine.Waiting{Reason: machine.WaitApproval, Environment: m.o.Environment}); err != nil {
					return sb, raw, nil, err
				}
			}
			if time.Since(asked) >= m.o.ApprovalWait {
				return sb, raw, nil, &machine.Waiting{Reason: machine.WaitApproval, Environment: m.o.Environment,
					Err: fmt.Errorf("the computer's owner has not answered for %s; the create still waits on the computer", m.o.ApprovalWait)}
			}
			// A sandbox the owner allows starts afresh within its own window.
			deadline = time.Now().Add(m.o.StartTimeout)
		case sb.Status.Phase == "Stopped" || sb.Status.Phase == "Failed":
			started, restart, err := m.running(ctx, sb)
			if errors.Is(err, errFailed) || errors.Is(err, errGone) || errors.Is(err, errFailedAgain) {
				if _, derr := m.c.Delete(ctx, client.KindSandbox, sb.Status.ID); derr != nil && client.CodeOf(derr) != "not_found" {
					return sb, raw, nil, refused("delete the failed sandbox "+m.name, derr)
				}
				return sb, raw, nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
			}
			if err != nil {
				return sb, raw, nil, m.offline(err)
			}
			_, raw, err = m.c.GetSandbox(ctx, started.Status.ID)
			if err != nil {
				return sb, raw, nil, m.offline(refused("read the sandbox "+m.name, err))
			}
			return started, raw, restart, nil
		case !time.Now().Before(deadline):
			return sb, raw, nil, fmt.Errorf("%w: the sandbox %s is %s after %s", ErrUnavailable, m.name, sb.Status.Phase, m.o.StartTimeout)
		}
		if err := sleep(ctx, m.o.PollInterval); err != nil {
			return sb, raw, nil, err
		}
		next, nraw, err := m.c.GetSandbox(ctx, sb.Status.ID)
		switch {
		case client.CodeOf(err) == "not_found":
			return sb, raw, nil, fmt.Errorf("%w: the sandbox %s is gone", ErrUnavailable, m.name)
		case err != nil:
			return sb, raw, nil, m.offline(refused("read the sandbox "+m.name, err))
		}
		sb, raw = next, nraw
	}
}

// readHost reads what the device's environment reports of the computer:
// its operating system and architecture, which pick the helper's build,
// and the isolation it confines a sandbox with. An environment that is
// not Ready, or whose read the control plane cannot serve, is a computer
// that is offline. An environment the session's credential may not read
// leaves the host to the sandbox's own report: the helper is then picked
// by what the sandbox's uname says.
func (m *Machine) readHost(ctx context.Context) error {
	env, _, err := m.c.GetEnvironment(ctx, m.o.Environment)
	switch code := client.CodeOf(err); {
	case code == "forbidden" || code == "not_found":
		return nil
	case err != nil:
		return m.offline(refused("read the environment "+m.o.Environment, err))
	case env.Status.Phase != v1.EnvironmentReady:
		return &machine.Waiting{Reason: machine.WaitOffline, Environment: m.o.Environment,
			Err: fmt.Errorf("the environment %s is %s: %s", m.o.Environment, env.Status.Phase, env.Status.Reason)}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.isolation = env.Status.Isolation
	if len(env.Status.Hosts) > 0 {
		h := env.Status.Hosts[0].Host
		m.host = &h
	}
	return nil
}

// offline is err as machine.Waiting offline when Cella answered that the
// device's worker does not answer, and err as it is otherwise.
func (m *Machine) offline(err error) error {
	if client.CodeOf(err) != codeDriverUnavailable {
		return err
	}
	return &machine.Waiting{Reason: machine.WaitOffline, Environment: m.o.Environment, Err: err}
}

// createDevice creates the device's sandbox without waiting for it to
// run: the computer may hold the create while it asks its owner, which
// admitted waits on. A create that loses a race with another runner's
// takes the sandbox that runner made.
func (m *Machine) createDevice(ctx context.Context) (v1.Sandbox, []byte, bool, error) {
	body, err := m.manifest(ctx)
	if err != nil {
		return v1.Sandbox{}, nil, false, err
	}
	sb, raw, err := m.c.CreateSandbox(ctx, client.JSON(body))
	created := true
	if client.CodeOf(err) == "name_taken" {
		sb, raw, err = m.c.GetSandbox(ctx, m.name)
		created = false
	}
	if err != nil {
		return v1.Sandbox{}, nil, false, m.offline(refused("create the sandbox "+m.name, err))
	}
	return sb, raw, created, nil
}

// heldBy reports whether the computer holds sb for its owner's answer:
// its HostApproval condition is Unknown, the state Cella spells a pending
// approval in.
func heldBy(sb v1.Sandbox) bool {
	if sb.Status.Phase != "Pending" {
		return false
	}
	c, ok := hostApproval(sb)
	return ok && c.Status == v1.ConditionUnknown
}

// hostApproval is sb's HostApproval condition.
func hostApproval(sb v1.Sandbox) (v1.Condition, bool) {
	for _, c := range sb.Status.Conditions {
		if c.Type == v1.ConditionHostApproval {
			return c, true
		}
	}
	return v1.Condition{}, false
}

// scratchOf is the temporary directory a sandbox's status.paths names: a
// driver that cannot remap paths, as a person's computer's cannot,
// reports its scratch directories by their real paths (Cella spec 081).
// It is read from the answer's own bytes, so a control plane whose
// manifest types this client predates still names it.
func scratchOf(raw []byte) (string, error) {
	var doc struct {
		Status struct {
			Paths *struct {
				Tmp string `json:"tmp"`
			} `json:"paths"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("read the sandbox's status: %w", err)
	}
	p := doc.Status.Paths
	switch {
	case p == nil || p.Tmp == "":
		return "", errors.New("the computer reports no temporary directory for the sandbox, which a device needs for its helper")
	case !path.IsAbs(p.Tmp) || path.Clean(p.Tmp) != p.Tmp:
		return "", fmt.Errorf("the computer reports the temporary directory %q, which is not an absolute clean path", p.Tmp)
	}
	return p.Tmp, nil
}

// probeDevice makes the spill and helper directories and reads the
// sandbox's platform and the SHA-256 of the helper already in it. It
// makes no home directory and configures no git: a device keeps the
// driver's own scratch home, and its folder is the person's own.
const probeDevice = `mkdir -p "$2" "$3" &&
uname -s && uname -m && if [ -x "$1" ]; then "$1" sum; fi`

// ensureDeviceHelper puts the helper build for the computer's operating
// system and architecture in the sandbox's scratch directory, unless the
// same build is there, through Cella's file route with mode 0755: a
// device's exec takes its input as an attached stage, and the file route
// writes the build whole. The computer's own report picks the build, and
// the sandbox's uname where the environment could not be read.
func (m *Machine) ensureDeviceHelper(ctx context.Context, id string) error {
	helper := m.helper()
	res, _, err := m.c.Exec(ctx, id, client.ExecRequest{
		Command: []string{shell, "-c", probeDevice, "sh", helper, m.SpillDir(), path.Dir(helper)},
		Timeout: "1m",
	})
	if err != nil {
		return m.offline(refused("probe the sandbox "+m.name, err))
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if res.ExitCode != 0 || len(lines) < 2 {
		return fmt.Errorf("%w: the sandbox %s did not report its platform: %q", ErrUnavailable, m.name, res.Stdout+res.Stderr)
	}
	goos, goarch := platform(lines[0], lines[1])
	m.mu.Lock()
	if m.host != nil {
		goos, goarch = m.host.OS, m.host.Arch
	}
	m.mu.Unlock()
	bin, ok := m.o.Helpers[goos+"/"+goarch]
	if !ok {
		return fmt.Errorf("%w: the runner carries no topos-machine build for %s/%s", ErrUnavailable, goos, goarch)
	}
	sum := sha256.Sum256(bin)
	want := hex.EncodeToString(sum[:])
	if len(lines) < 3 || strings.TrimSpace(lines[2]) != want {
		if err := m.c.FilePut(ctx, id, helper, octal(0o755), bytes.NewReader(bin)); err != nil {
			return m.offline(refused("write the helper to "+m.name, err))
		}
		res, _, err := m.c.Exec(ctx, id, client.ExecRequest{Command: []string{helper, "sum"}, Timeout: "1m"})
		switch {
		case err != nil:
			return m.offline(refused("run the helper in "+m.name, err))
		case res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != want:
			return fmt.Errorf("%w: the helper written to %s for %s/%s does not run: exit %d: %s", ErrUnavailable, m.name, goos, goarch, res.ExitCode, strings.TrimSpace(res.Stdout+res.Stderr))
		}
	}
	m.mu.Lock()
	m.info.OS, m.info.Arch = goos, goarch
	m.mu.Unlock()
	return nil
}

// deviceSpec shapes a device's create from a sandbox's (spec 080): no
// image, since the computer runs its own tools; no Secret and no
// variable of the machine's, HOME among them, so the sandbox keeps the
// driver's own scratch home; the session's folder as the host workspace
// and the working directory; no resources, which a
// computer does not size; and the annotations.
func (m *Machine) deviceSpec(sb *v1.Sandbox) {
	sb.Spec.Image, sb.Spec.Secrets = "", nil
	sb.Spec.Env = maps.Clone(m.o.Env)
	sb.Spec.Workspace = v1.Workspace{Host: m.o.Workdir}
	sb.Spec.Resources = v1.Resources{}
	sb.Metadata.Annotations = maps.Clone(m.o.Annotations)
}

// Absent reports a device whose computer stopped answering, or whose
// sandbox was lost under a call, as machine.Waiting offline: the session
// waits for the computer rather than going on without it (spec 080). Any
// other machine is never absent.
func (m *Machine) Absent() *machine.Waiting {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.o.Device || !m.lost || m.released {
		return nil
	}
	return &machine.Waiting{Reason: machine.WaitOffline, Environment: m.o.Environment}
}

// Device is a person's own computer as a session's machine (spec 080): a
// Cella machine opened with Options.Device, offering the operations of a
// machine and none of the recovery of a sandbox. It never says what its
// open made of it (machine.Recovering) and takes no archive
// (machine.Importer), so a runner restores nothing into it and saves no
// copy of the person's folder; it reaches no connection service; and it
// is never created again in the place of one that was lost, only found or
// created on the same computer, in the same folder, by the next open.
type Device struct {
	m *Machine
}

// OpenDevice opens a device's machine: o.Device is set for the caller.
func OpenDevice(ctx context.Context, o Options) (*Device, error) {
	o.Device = true
	m, err := Open(ctx, o)
	if err != nil {
		return nil, err
	}
	return &Device{m: m}, nil
}

// Machine is the Cella machine under the device, for a test that reaches
// what the device does not offer.
func (d *Device) Machine() *Machine { return d.m }

func (d *Device) Info() machine.Info       { return d.m.Info() }
func (d *Device) Roots() []string          { return d.m.Roots() }
func (d *Device) SpillDir() string         { return d.m.SpillDir() }
func (d *Device) Absent() *machine.Waiting { return d.m.Absent() }

func (d *Device) Exec(ctx context.Context, r machine.ExecRequest) (machine.ExecResult, error) {
	return d.m.Exec(ctx, r)
}

func (d *Device) ExecStream(ctx context.Context, r machine.ExecRequest) (machine.ExecStream, error) {
	return d.m.ExecStream(ctx, r)
}

func (d *Device) ReadFile(ctx context.Context, p string) (io.ReadCloser, error) {
	return d.m.ReadFile(ctx, p)
}

func (d *Device) WriteFile(ctx context.Context, p string, r io.Reader, mode fs.FileMode) error {
	return d.m.WriteFile(ctx, p, r, mode)
}

func (d *Device) Stat(ctx context.Context, p string) (machine.FileInfo, error) {
	return d.m.Stat(ctx, p)
}

func (d *Device) List(ctx context.Context, p string) ([]machine.FileInfo, error) {
	return d.m.List(ctx, p)
}

func (d *Device) Remove(ctx context.Context, p string) error { return d.m.Remove(ctx, p) }

func (d *Device) Rename(ctx context.Context, from, to string) error {
	return d.m.Rename(ctx, from, to)
}

func (d *Device) Search(ctx context.Context, q machine.SearchRequest) (machine.SearchResult, error) {
	return d.m.Search(ctx, q)
}

func (d *Device) Fetch(ctx context.Context, r machine.FetchRequest) (machine.FetchResult, error) {
	return d.m.Fetch(ctx, r)
}

// Release lets go of the device; at the session's end it deletes the
// sandbox, which on the computer removes its scratch directory alone and
// never the person's folder.
func (d *Device) Release(ctx context.Context, end bool) error { return d.m.Release(ctx, end) }

// ApplyConnections gives a device nothing: its git and its environment
// are the person's own, and the session's connections are reached from
// the runner, not from the computer.
func (d *Device) ApplyConnections(context.Context, []machine.HTTPConnection) error { return nil }

// ReachesConnections is false: a device mounts no connection service.
func (d *Device) ReachesConnections() bool { return false }

var (
	_ machine.Machine   = (*Device)(nil)
	_ machine.Fetcher   = (*Device)(nil)
	_ machine.Connected = (*Device)(nil)
	_ machine.Absent    = (*Device)(nil)
)
