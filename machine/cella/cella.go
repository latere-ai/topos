// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package cella is the Cella machine of spec 009: one Cella sandbox per
// session, reached through latere.ai/x/cella/client. The sandbox is
// named after the session, so a runner that claims the session later
// finds it by name, starts it when Cella stopped it for idleness or when
// it failed, and creates it again when it is gone. A failed sandbox is
// deleted only when Cella refuses to start it, since the delete is what
// removes its workspace.
//
// Files inside the sandbox's workspace go through Cella's file routes.
// Everything else goes through the helper topos-machine, which the
// machine uploads into the sandbox: commands run under it in their own
// process group with a timeout, an input that ends and a report of their
// final directory, grep and glob run in it where the files are, and the
// files outside the workspace, such as the spill directory, are reached
// through it, because Cella's file routes serve the workspace alone.
package cella

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"latere.ai/x/cella/client"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/pkg/otel"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// The labels every sandbox and every Secret made for it carry (spec 009,
// spec 018): an authorizer that binds a session's token to the session
// reads LabelSession to know what the session made.
const (
	LabelSession = "topos.latere.ai/session"
	LabelAgent   = "topos.latere.ai/agent"
)

// Labels are the labels of a session's sandbox and of each Secret made
// for it: the installation's, TOPOS_CELLA_LABELS, and the session's and
// the agent's, which win over one of the same key. An empty agent adds no
// agent label. installation is not changed.
func Labels(installation map[string]string, session, agent string) map[string]string {
	labels := maps.Clone(installation)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[LabelSession] = session
	if agent != "" {
		labels[LabelAgent] = agent
	}
	return labels
}

// The lifecycle of spec 009: Cella stops an idle sandbox after AutoStop
// and keeps its workspace; a stopped sandbox is never deleted for being
// stopped, so it lasts until the session ends or its ttl passes.
const (
	AutoStop   = 15 * time.Minute
	autoDelete = v1.DurationNever
)

// DefaultDir is where the helper and the spill directory live inside a
// sandbox, outside the workspace.
const DefaultDir = "/tmp/topos"

// Defaults of the waits.
const (
	DefaultStartTimeout = 5 * time.Minute
	DefaultPollInterval = time.Second
)

// MaxOutput bounds what Exec keeps of one command's output, as on the
// host machine; the tool layer spills anything past its own cap.
const MaxOutput = 64 << 20

// Options configure a Cella machine.
type Options struct {
	// URL is Cella's API, TOPOS_CELLA_URL. Empty refuses the machine with
	// machine_unavailable.
	URL string
	// Token is the session's own credential, asked once per request
	// (spec 018).
	Token client.TokenSource
	// HTTPClient carries every call, the exec socket's upgrade included.
	// Nil is an instrumented client that keeps to HTTP/1.1, which the
	// upgrade needs, and bounds no call: a create held until the sandbox
	// runs and a command's session are bounded by their contexts.
	HTTPClient *http.Client

	// Session is the session's id, ses_ and a ULID; the sandbox is named
	// after it.
	Session string
	// Agent is the agent's name, the value of the agent label.
	Agent string
	// Environment is the session's machine.environment, which may name
	// an Environment whose workers the developer runs; empty is Cella's
	// default.
	Environment string
	// Image is the agent's spec.machine.image; empty is Cella's default.
	Image string
	// Resources is the agent's spec.machine.resources.
	Resources v1.Resources
	// EgressMode is the mode of the sandbox's egress, open, allowlist or
	// none, the session's network's (spec 052); empty is allowlist.
	EgressMode string
	// Egress are the hosts the sandbox may reach under allowlist: the
	// session's network, the agent's spec.machine.egress and the git host
	// of the session's repositories. The hosts of the named secrets join
	// them. They are not sent under open or none.
	Egress []string
	// Secrets are the session's named secrets, each under the variable
	// its placeholder arrives in.
	Secrets []v1.SecretMount
	// Env is the sandbox's environment beside the secrets' placeholders,
	// such as the model gateway's URL a sandbox's own model key is for;
	// it never holds a credential.
	Env map[string]string
	// Labels are the installation's labels, TOPOS_CELLA_LABELS, which the
	// sandbox carries beside the session's and the agent's, such as the
	// tenant a Cella's authorizer places what a caller creates in. The
	// session's and the agent's labels win over one of the same key.
	Labels map[string]string
	// TTL is the session's remaining age, the sandbox's backstop; zero
	// leaves Cella's default.
	TTL time.Duration
	// Workdir is the working directory inside the sandbox; empty is the
	// sandbox's own, its workspace.
	Workdir string

	// Helpers are the topos-machine builds by platform, "linux/amd64"
	// and "linux/arm64"; the runner embeds them. The machine uploads the
	// one of the sandbox's platform.
	Helpers map[string][]byte
	// Dir holds the helper (bin/) and the spill directory (spill/)
	// inside the sandbox, outside its workspace; empty is DefaultDir.
	Dir string

	// StartTimeout bounds a create or a start waiting for the sandbox to
	// run; zero is DefaultStartTimeout.
	StartTimeout time.Duration
	// PollInterval is how often a wait reads the sandbox again; zero is
	// DefaultPollInterval.
	PollInterval time.Duration
}

// Machine is a Cella sandbox as a session's machine.
type Machine struct {
	c    *client.Client
	o    Options
	name string

	// wake serializes finding, starting and creating the sandbox.
	wake sync.Mutex

	mu        sync.Mutex
	id        string
	info      machine.Info
	workspace string
	roots     []string
	created   bool
	// mounts are the Secrets the sandbox mounts as Cella holds it, which
	// for a sandbox found by name are the ones its create named.
	mounts []v1.SecretMount
	// restart is the restart the last attach made, nil when it found the
	// sandbox running, stopped or absent.
	restart *machine.Restart
	// onRestart is the hook told of each restart a call makes after the
	// attach.
	onRestart func(ctx context.Context, r machine.Restart) error
	lost      bool
	released  bool
	// asked is the egress this machine last created or updated the
	// sandbox with, which an ApplyNetwork of the same network does not
	// send again; nil until it asked one.
	asked *v1.Egress
	// secretHosts are the hosts of the session's named secrets, read from
	// Cella once.
	secretHostList []string
	secretsRead    bool
}

// SandboxName is the sandbox of a session: ses- and the session's ULID
// in lowercase, a DNS label Cella accepts as a name.
func SandboxName(sessionID string) string {
	return "ses-" + strings.ToLower(strings.TrimPrefix(sessionID, session.PrefixSession))
}

// Open finds the session's sandbox by name or creates it, holds until it
// runs, and puts the helper in it. Created reports which it was.
func Open(ctx context.Context, o Options) (*Machine, error) {
	if o.URL == "" {
		return nil, fmt.Errorf("%w: TOPOS_CELLA_URL is not set", ErrUnavailable)
	}
	if err := session.CheckID(session.PrefixSession, o.Session); err != nil {
		return nil, fmt.Errorf("machine: the session id %q: %w", o.Session, err)
	}
	if len(o.Helpers) == 0 {
		return nil, fmt.Errorf("%w: the runner carries no topos-machine build to upload", ErrUnavailable)
	}
	if o.Dir == "" {
		o.Dir = DefaultDir
	}
	if !path.IsAbs(o.Dir) || path.Clean(o.Dir) != o.Dir {
		return nil, fmt.Errorf("machine: the helper directory %q is not an absolute clean path", o.Dir)
	}
	if o.StartTimeout <= 0 {
		o.StartTimeout = DefaultStartTimeout
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = httpClient()
	}
	c, err := client.New(client.Config{URL: o.URL, Token: o.Token, HTTPClient: hc, UserAgent: "topos"})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	m := &Machine{c: c, o: o, name: SandboxName(o.Session)}
	m.wake.Lock()
	defer m.wake.Unlock()
	if err := m.attach(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

// httpClient is the default carrier: traced, and on HTTP/1.1, because the
// exec socket is an upgrade and a connection that negotiated HTTP/2 has
// nothing to upgrade.
func httpClient() *http.Client {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	base := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        16,
		IdleConnTimeout:     90 * time.Second,
		Protocols:           protocols,
	}
	return &http.Client{Transport: otel.Transport(base)}
}

// Name is the sandbox's name.
func (m *Machine) Name() string { return m.name }

// Created reports whether the last Open or Recreate created the sandbox
// rather than finding it. A runner that expected the session's sandbox
// and finds Created set knows its files are gone: it restores the latest
// checkpoint and appends session.machine with reason restored, or reason
// replaced and a session.error machine_lost (spec 009).
func (m *Machine) Created() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.created
}

// Restarted reports the restart the last Open or Recreate made: it found
// the sandbox failed and Cella started it again on the same workspace, so
// its files are kept and every process that ran in it is gone.
func (m *Machine) Restarted() (machine.Restart, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.restart == nil {
		return machine.Restart{}, false
	}
	return *m.restart, true
}

// OnRestart sets the hook called after each restart a call makes once
// the machine is open: a call that found the sandbox failed and had it
// started again, or a command whose sandbox stopped under it and came
// back. The hook runs when the sandbox runs again with the helper in it,
// before the call goes on, and its failure is the call's.
func (m *Machine) OnRestart(hook func(ctx context.Context, r machine.Restart) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onRestart = hook
}

// tell hands a restart to the hook, when one is set.
func (m *Machine) tell(ctx context.Context, r machine.Restart) error {
	m.mu.Lock()
	hook := m.onRestart
	m.mu.Unlock()
	if hook == nil {
		return nil
	}
	return hook(ctx, r)
}

// Recreate brings the session's sandbox back after an operation answered
// ErrLost, as Open does: a sandbox that is gone is created again with a
// fresh workspace, and one that failed is started again when Cella takes
// the start, and replaced only when it does not. The helper is uploaded,
// the machine is usable again, and Info names the sandbox it has.
// Restoring the files of a replaced one is the runner's, through
// ImportTar.
func (m *Machine) Recreate(ctx context.Context) error {
	m.wake.Lock()
	defer m.wake.Unlock()
	m.mu.Lock()
	released := m.released
	m.mu.Unlock()
	if released {
		return machine.ErrReleased
	}
	if err := m.attach(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	m.lost = false
	m.mu.Unlock()
	return nil
}

// Mounts are the Secrets the sandbox mounts, as Cella answered it at the
// last Open or Recreate: a sandbox found by name mounts what its create
// named, which may not be what this machine's Options name, since Cella
// takes a sandbox's mounts at its create alone.
func (m *Machine) Mounts() []v1.SecretMount {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.mounts)
}

func (m *Machine) Info() machine.Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.info
}

func (m *Machine) Roots() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.roots)
}

func (m *Machine) SpillDir() string { return path.Join(m.o.Dir, "spill") }

// HomeDir is the sandbox's HOME unless Options.Env names one: Cella
// mounts the root filesystem read-only, so the image's home directory
// cannot hold what git, package managers and build caches write there,
// and the workspace is the person's tree, not a place for them.
func (m *Machine) HomeDir() string { return path.Join(m.o.Dir, "home") }

// helper is the helper's path inside the sandbox.
func (m *Machine) helper() string { return path.Join(m.o.Dir, "bin", "topos-machine") }

// usable refuses a call after Release, or while the sandbox is lost.
func (m *Machine) usable() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.released:
		return "", machine.ErrReleased
	case m.lost:
		return "", fmt.Errorf("%w: %s", ErrLost, m.name)
	}
	return m.id, nil
}

// Release lets go of the machine. An idle session keeps its sandbox,
// which Cella stops after AutoStop with its workspace kept; at the
// session's end the sandbox is deleted, and every call after answers
// ErrReleased.
func (m *Machine) Release(ctx context.Context, end bool) error {
	if !end {
		return nil
	}
	m.mu.Lock()
	id, released := m.id, m.released
	m.mu.Unlock()
	if released {
		return nil
	}
	if _, err := m.c.Delete(ctx, client.KindSandbox, id); err != nil && client.CodeOf(err) != "not_found" {
		return fmt.Errorf("machine: delete the sandbox %s: %w", m.name, err)
	}
	m.mu.Lock()
	m.released = true
	m.mu.Unlock()
	return nil
}

// attach finds the sandbox by name, starting it when it is stopped or
// failed, or creates it, and makes sure the helper is in it. The helper
// lives outside the workspace, so a start may have lost it.
func (m *Machine) attach(ctx context.Context) error {
	sb, _, err := m.c.GetSandbox(ctx, m.name)
	created := false
	var restart *machine.Restart
	switch {
	case err == nil:
		sb, restart, err = m.running(ctx, sb)
		if errors.Is(err, errFailed) {
			// Cella starts no failed sandbox, or not this one. Only then
			// is it replaced: the delete removes its workspace, so the
			// session's files are gone, which the runner records as a
			// loss.
			if _, derr := m.c.Delete(ctx, client.KindSandbox, sb.Status.ID); derr != nil && client.CodeOf(derr) != "not_found" {
				return refused("delete the failed sandbox "+m.name, derr)
			}
			sb, err = m.gone(ctx, sb.Status.ID)
		}
		if errors.Is(err, errGone) {
			sb, created, err = m.create(ctx)
		}
	case client.CodeOf(err) == "not_found":
		sb, created, err = m.create(ctx)
	default:
		err = refused("find the sandbox "+m.name, err)
	}
	if err != nil {
		return err
	}
	// A sandbox found by name runs the network it was last given, which a
	// change made while no runner held the session has moved since.
	if !created {
		if sb, err = m.reconcile(ctx, sb); err != nil {
			return err
		}
	}
	m.set(sb, created, restart)
	return m.ensureHelper(ctx, sb.Status.ID)
}

// create creates the sandbox and holds until it runs. A create that loses
// a race with another runner's takes the sandbox that runner made.
func (m *Machine) create(ctx context.Context) (v1.Sandbox, bool, error) {
	body, err := m.manifest(ctx)
	if err != nil {
		return v1.Sandbox{}, false, err
	}
	sb, _, err := m.c.CreateSandbox(ctx, client.JSON(body), client.Wait(m.o.StartTimeout))
	created := true
	if client.CodeOf(err) == "name_taken" {
		sb, _, err = m.c.GetSandbox(ctx, m.name)
		created = false
	}
	if err != nil {
		return v1.Sandbox{}, false, refused("create the sandbox "+m.name, err)
	}
	// A sandbox just created holds nothing a restart could keep, so a
	// start that brought it up is its creation.
	sb, _, err = m.running(ctx, sb)
	if errors.Is(err, errFailed) || errors.Is(err, errFailedAgain) || errors.Is(err, errGone) {
		return v1.Sandbox{}, false, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return sb, created, err
}

// set records the running sandbox, and how the attach found it.
func (m *Machine) set(sb v1.Sandbox, created bool, restart *machine.Restart) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.id = sb.Status.ID
	m.created = created
	m.restart = restart
	m.mounts = slices.Clone(sb.Spec.Secrets)
	m.workspace = sb.Spec.Workspace.Path
	workdir := cmpOr(m.o.Workdir, sb.Spec.Workdir, m.workspace)
	m.info = machine.Info{Kind: machine.KindCella, ID: sb.Status.ID, Workdir: workdir, Environment: sb.Status.Environment,
		OS: m.info.OS, Arch: m.info.Arch, Egress: string(sb.Spec.Network.Egress.Mode)}
	m.roots = nil
	for _, r := range []string{workdir, m.workspace, m.SpillDir()} {
		if r != "" && !slices.Contains(m.roots, r) {
			m.roots = append(m.roots, r)
		}
	}
}

func cmpOr(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// reasonOutOfMemory is the reason Cella gives a sandbox whose container
// was killed for memory.
const reasonOutOfMemory = "OOMKilled"

// Errors of a wait for a sandbox to run.
var (
	// errFailed is a failed sandbox Cella refuses to start: a core through
	// v0.12.0 starts none, and no core starts one whose failure left no
	// workspace.
	errFailed = errors.New("the sandbox failed and Cella does not start it again")
	// errFailedAgain is a failed sandbox that Cella started and that failed
	// again before it ran. Nothing of it is deleted, and the next call
	// starts it again.
	errFailedAgain = errors.New("machine: the sandbox failed again right after it was started; nothing of it was deleted, and the next call starts it again")
	errGone        = errors.New("the sandbox is gone")
	// errStoppedAgain is a sandbox that a call started and that did not
	// run any more when the call's operation went again.
	errStoppedAgain = errors.New("machine: the sandbox stopped again right after it was started; nothing of it was deleted, and the next call starts it again")
)

// running holds until the sandbox runs: a stopped one is started, one on
// its way up or down is read again until it settles, and a deleted one is
// errGone.
//
// A failed one is started too, and the restart is returned: a core after
// v0.12.0 starts a failed sandbox whose workspace is kept, on that
// workspace. One wait starts a sandbox out of Failed once. One that fails
// again after the start is errFailedAgain, so a sandbox that dies as soon
// as it is up cannot spin. A start refused with phase_conflict is read
// again, since a start that raced another caller's, or the core's own
// restart, answers the same: a sandbox on its way up is waited for as a
// restart, and one that still reads Failed is asked once more, so a
// sandbox that failed again in that moment is not taken for one Cella
// will not start. The second refusal is errFailed, the only answer a
// caller replaces the sandbox for.
func (m *Machine) running(ctx context.Context, sb v1.Sandbox) (v1.Sandbox, *machine.Restart, error) {
	deadline := time.Now().Add(m.o.StartTimeout)
	var restart *machine.Restart
	declined := false
	for {
		switch sb.Status.Phase {
		case "Running":
			return sb, restart, nil
		case "Failed":
			reason := cmpOr(sb.Status.Reason, "no reason given")
			if restart != nil {
				return sb, nil, fmt.Errorf("%w: %s: %s", errFailedAgain, m.name, reason)
			}
			failed := machine.Restart{OutOfMemory: sb.Status.Reason == reasonOutOfMemory}
			started, _, err := m.c.StartSandbox(ctx, sb.Status.ID)
			switch {
			case err == nil:
				sb, restart = started, &failed
				continue
			case client.CodeOf(err) == "not_found":
				return sb, nil, errGone
			case client.CodeOf(err) != "phase_conflict":
				return sb, nil, refused("start the failed sandbox "+m.name, err)
			}
			next, _, err := m.c.GetSandbox(ctx, sb.Status.ID)
			switch {
			case client.CodeOf(err) == "not_found":
				return sb, nil, errGone
			case err != nil:
				return sb, nil, refused("read the sandbox "+m.name, err)
			case next.Status.Phase != "Failed":
				// Another start took it out of Failed first.
				sb, restart = next, &failed
				continue
			case declined:
				return next, nil, fmt.Errorf("%w: %s: %s", errFailed, m.name, reason)
			}
			sb, declined = next, true
			continue
		case "Stopped":
			started, _, err := m.c.StartSandbox(ctx, sb.Status.ID)
			switch {
			case err == nil:
				sb = started
				continue
			case client.CodeOf(err) == "not_found":
				return sb, nil, errGone
			case client.CodeOf(err) != "phase_conflict":
				// A conflict is a start that raced another; the read
				// below settles it.
				return sb, nil, refused("start the sandbox "+m.name, err)
			}
		}
		if !time.Now().Before(deadline) {
			return sb, nil, fmt.Errorf("%w: the sandbox %s is %s after %s", ErrUnavailable, m.name, sb.Status.Phase, m.o.StartTimeout)
		}
		if err := sleep(ctx, m.o.PollInterval); err != nil {
			return sb, nil, err
		}
		next, _, err := m.c.GetSandbox(ctx, sb.Status.ID)
		if client.CodeOf(err) == "not_found" {
			return sb, nil, errGone
		}
		if err != nil {
			return sb, nil, refused("read the sandbox "+m.name, err)
		}
		sb = next
	}
}

// gone waits for a deleted sandbox to be gone, so its name is free.
func (m *Machine) gone(ctx context.Context, id string) (v1.Sandbox, error) {
	deadline := time.Now().Add(m.o.StartTimeout)
	for {
		sb, _, err := m.c.GetSandbox(ctx, id)
		if client.CodeOf(err) == "not_found" {
			return sb, errGone
		}
		if err != nil {
			return sb, refused("read the sandbox "+m.name, err)
		}
		if !time.Now().Before(deadline) {
			return sb, fmt.Errorf("%w: the sandbox %s is still %s after %s", ErrUnavailable, m.name, sb.Status.Phase, m.o.StartTimeout)
		}
		if err := sleep(ctx, m.o.PollInterval); err != nil {
			return sb, err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// manifest is the sandbox's create body (spec 009): named after the
// session, labeled, on the session's Environment, with the session's
// network as its egress, the named secrets and the lifecycle.
func (m *Machine) manifest(ctx context.Context) ([]byte, error) {
	m.mu.Lock()
	mode, hosts := m.o.EgressMode, slices.Clone(m.o.Egress)
	m.mu.Unlock()
	egress, err := m.egress(ctx, mode, hosts)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.asked = &egress
	m.mu.Unlock()
	labels := Labels(m.o.Labels, m.o.Session, m.o.Agent)
	env := maps.Clone(m.o.Env)
	if _, set := env["HOME"]; !set {
		if env == nil {
			env = map[string]string{}
		}
		env["HOME"] = m.HomeDir()
	}
	life := v1.Lifecycle{AutoStop: duration(AutoStop), AutoDelete: autoDelete}
	if m.o.TTL > 0 {
		life.TTL = duration(m.o.TTL)
		if m.o.TTL < AutoStop {
			// Cella holds an idle stop inside the sandbox's life.
			life.AutoStop = life.TTL
		}
	}
	sb := v1.Sandbox{
		APIVersion: v1.APIVersion,
		Kind:       v1.KindSandbox,
		Metadata:   v1.Metadata{Name: m.name, Labels: labels},
		Spec: v1.SandboxSpec{
			Environment: m.o.Environment,
			Image:       m.o.Image,
			Workdir:     m.o.Workdir,
			Resources:   m.o.Resources,
			Env:         env,
			Secrets:     m.o.Secrets,
			Network:     v1.Network{Egress: egress},
			Lifecycle:   life,
		},
	}
	return json.Marshal(sb)
}

// duration renders a duration in whole seconds, rounded up, which Cella's
// duration syntax reads.
func duration(d time.Duration) v1.Duration {
	s := int64((d + time.Second - 1) / time.Second)
	return v1.Duration(strconv.FormatInt(s, 10) + "s")
}

// probe makes the spill and home directories, marks the workspace a safe
// directory in git's global configuration, and reads the sandbox's
// platform and the SHA-256 of the helper already in it, which is absent
// after a create and may be after a start: /tmp does not outlive a stop on
// every driver, so the home directory and its configuration are made on
// every open. The workspace volume's root belongs to another user than the
// sandbox's process, which git refuses to work in unless it is named safe.
const probe = `mkdir -p "$2" "$3" &&
if [ -n "$4" ] && command -v git >/dev/null 2>&1; then
  git config --global --get-all safe.directory | grep -qxF "$4" || git config --global --add safe.directory "$4"
fi &&
uname -s && uname -m && if [ -x "$1" ]; then "$1" sum; fi`

// upload writes the helper from the session's input, which carries
// exactly as many bytes as head reads, because the exec socket cannot
// end an input, and answers the new helper's SHA-256.
const upload = `set -e
mkdir -p "$1"
t="$1/.topos-machine.$$"
trap 'rm -f "$t"' EXIT
head -c "$2" >"$t"
chmod 755 "$t"
mv -f "$t" "$1/topos-machine"
"$1/topos-machine" sum`

// ensureHelper makes the spill directory, puts the helper build of the
// sandbox's platform in the sandbox unless the same build is already
// there, and records the platform.
func (m *Machine) ensureHelper(ctx context.Context, id string) error {
	res, _, err := m.c.Exec(ctx, id, client.ExecRequest{
		Command: []string{shell, "-c", probe, "sh", m.helper(), m.SpillDir(), m.HomeDir(), m.workspace},
		Timeout: "1m",
	})
	if err != nil {
		return refused("probe the sandbox "+m.name, err)
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if res.ExitCode != 0 || len(lines) < 2 {
		return fmt.Errorf("%w: the sandbox %s did not report its platform: %q", ErrUnavailable, m.name, res.Stdout+res.Stderr)
	}
	goos, goarch := platform(lines[0], lines[1])
	bin, ok := m.o.Helpers[goos+"/"+goarch]
	if !ok {
		return fmt.Errorf("%w: the runner carries no topos-machine build for %s/%s", ErrUnavailable, goos, goarch)
	}
	sum := sha256.Sum256(bin)
	want := hex.EncodeToString(sum[:])
	if len(lines) < 3 || strings.TrimSpace(lines[2]) != want {
		if err := m.put(ctx, id, bin, want); err != nil {
			return err
		}
	}
	m.mu.Lock()
	m.info.OS, m.info.Arch = goos, goarch
	m.mu.Unlock()
	return nil
}

// put uploads the helper through an exec session's input.
func (m *Machine) put(ctx context.Context, id string, bin []byte, want string) error {
	s, err := m.c.ExecSession(ctx, id, client.ExecRequest{
		Command: []string{shell, "-c", upload, "sh", path.Dir(m.helper()), strconv.Itoa(len(bin))},
		Timeout: "5m",
	})
	if err != nil {
		return refused("upload the helper to "+m.name, err)
	}
	sent := make(chan error, 1)
	go func() {
		for b := bin; len(b) > 0; {
			n := min(len(b), chunk)
			if _, err := s.Write(b[:n]); err != nil {
				sent <- err
				return
			}
			b = b[n:]
		}
		sent <- nil
	}()
	out, rerr := readAll(s, 1<<16)
	code, werr := s.Wait()
	err = errors.Join(rerr, werr, <-sent, closeSession(s))
	if err != nil {
		return fmt.Errorf("%w: upload the helper to %s: %w", ErrUnavailable, m.name, err)
	}
	if code != 0 || strings.TrimSpace(string(out)) != want {
		return fmt.Errorf("%w: upload the helper to %s: exit %d: %s", ErrUnavailable, m.name, code, strings.TrimSpace(string(out)))
	}
	return nil
}

// platform maps uname's answers to Go's names.
func platform(sys, arch string) (string, string) {
	goos := strings.ToLower(strings.TrimSpace(sys))
	switch a := strings.TrimSpace(arch); a {
	case "x86_64", "amd64":
		return goos, "amd64"
	case "aarch64", "arm64":
		return goos, "arm64"
	default:
		return goos, a
	}
}

// wakeUp brings the sandbox back after a call found it not running, or
// found no helper in it: it starts the sandbox, a failed one included,
// holds until it runs, and puts the helper back. A restart out of a
// failure is told to the hook before the call goes on. A sandbox that is
// gone, or that failed and Cella does not start again, marks the machine
// lost: a call deletes nothing.
func (m *Machine) wakeUp(ctx context.Context, id string) error {
	restart, err := m.wake1(ctx, id)
	if err != nil || restart == nil {
		return err
	}
	return m.tell(ctx, *restart)
}

// wake1 is wakeUp's work under the wake lock, and returns the restart it
// made, nil for a sandbox it found running or stopped.
func (m *Machine) wake1(ctx context.Context, id string) (*machine.Restart, error) {
	m.wake.Lock()
	defer m.wake.Unlock()
	m.mu.Lock()
	current := m.id
	m.mu.Unlock()
	if current != id {
		// Another call already brought a sandbox back.
		return nil, nil
	}
	sb, _, err := m.c.GetSandbox(ctx, id)
	if client.CodeOf(err) == "not_found" {
		return nil, m.markLost()
	}
	if err != nil {
		return nil, fmt.Errorf("machine: read the sandbox %s: %w", m.name, err)
	}
	sb, restart, err := m.running(ctx, sb)
	switch {
	case errors.Is(err, errGone):
		return nil, m.markLost()
	case errors.Is(err, errFailed):
		return nil, fmt.Errorf("%w: %w", m.markLost(), err)
	case err != nil:
		return nil, err
	}
	return restart, m.ensureHelper(ctx, sb.Status.ID)
}

// ended says what became of a command whose session closed with no exit
// from the helper: code is the exit code the session ended with, -1 when
// it gave none, and cause the session's end as an error.
//
// A sandbox that runs and names no failure lost the command's helper
// alone: a kill is reported as one, with true, and anything else is
// cause. A sandbox that stopped, or that Cella says ran out of memory,
// took the command with it, and is brought back within this call, so the
// command's result says what is true of its files: the Stop is restarted
// when the sandbox runs again, lost when it is gone or Cella does not
// start it, and carries the error otherwise. A restart is told to the
// hook, as wakeUp's is.
func (m *Machine) ended(ctx context.Context, id string, code int, cause error) (*machine.Stop, bool, error) {
	if se, ok := spent("act in the sandbox "+m.name, cause); ok {
		return nil, false, se
	}
	// The command may have ended with its call's context; what became of
	// the machine is read and recorded all the same.
	settled := context.WithoutCancel(ctx)
	sb, _, err := m.c.GetSandbox(settled, id)
	switch {
	case client.CodeOf(err) == "not_found":
		// The marking is what the call answers: the Stop says it.
		_ = m.markLost()
		return &machine.Stop{Lost: true}, false, nil
	case err != nil:
		return nil, false, errors.Join(cause, fmt.Errorf("machine: read the sandbox %s: %w", m.name, err))
	}
	oom := sb.Status.Reason == reasonOutOfMemory
	if sb.Status.Phase == "Running" && !oom {
		if code == machine.ExitKilled {
			return nil, true, nil
		}
		return nil, false, cause
	}
	stop := &machine.Stop{OutOfMemory: oom}
	restart, err := m.wake1(ctx, id)
	switch {
	case errors.Is(err, ErrLost):
		stop.Lost = true
	case err != nil:
		stop.Err = err
	default:
		stop.Restarted = true
		// The sandbox stopped under a command, so it restarted whether
		// this call's start or Cella's own brought it back.
		r := machine.Restart{OutOfMemory: oom}
		if restart != nil {
			r.OutOfMemory = r.OutOfMemory || restart.OutOfMemory
		}
		if err := m.tell(settled, r); err != nil {
			return nil, false, err
		}
	}
	return stop, false, nil
}

// markLost records that the sandbox is gone and answers ErrLost; every
// call answers it until Recreate.
func (m *Machine) markLost() error {
	m.mu.Lock()
	m.lost = true
	m.mu.Unlock()
	return fmt.Errorf("%w: %s", ErrLost, m.name)
}

// call runs one operation on the sandbox. A sandbox that does not run,
// one Cella stopped for idleness or one that failed, is started and the
// operation runs once more; a not_found is the sandbox's when the sandbox
// is gone, which is ErrLost, and the path's otherwise. One call wakes the
// sandbox once: when it stopped again before the operation ran, the call
// fails and the next one wakes it. retry is false for an operation whose
// request body cannot be sent twice.
func (m *Machine) call(ctx context.Context, retry bool, f func(id string) error) error {
	id, err := m.usable()
	if err != nil {
		return err
	}
	err = f(id)
	if retry && (client.CodeOf(err) == "phase_conflict" || errors.Is(err, errNoHelper)) {
		if werr := m.wakeUp(ctx, id); werr != nil {
			return werr
		}
		if id, err = m.usable(); err != nil {
			return err
		}
		if err = f(id); client.CodeOf(err) == "phase_conflict" {
			return fmt.Errorf("%w: %s: %w", errStoppedAgain, m.name, err)
		}
	}
	return m.check(ctx, id, err)
}

// check tells a sandbox that is gone from a path that is missing, and
// names a refusal for spend as one.
func (m *Machine) check(ctx context.Context, id string, err error) error {
	if se, ok := spent("act in the sandbox "+m.name, err); ok {
		return se
	}
	if client.CodeOf(err) != "not_found" {
		return err
	}
	_, _, gerr := m.c.GetSandbox(ctx, id)
	switch {
	case client.CodeOf(gerr) == "not_found":
		return m.markLost()
	case gerr != nil:
		return errors.Join(err, gerr)
	}
	return err
}

var _ machine.Machine = (*Machine)(nil)
