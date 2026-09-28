// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package cella is the Cella machine of spec 009: one Cella sandbox per
// session, reached through latere.ai/x/cella/client. The sandbox is
// named after the session, so a runner that claims the session later
// finds it by name, starts it when Cella stopped it for idleness, and
// creates it again when it is gone.
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

// The labels every sandbox carries (spec 009).
const (
	LabelSession = "topos.latere.ai/session"
	LabelAgent   = "topos.latere.ai/agent"
)

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
	// Egress are the hosts the sandbox may reach: the agent's
	// spec.machine.egress and the git host of the session's repositories.
	// The hosts of the named secrets join them.
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
	lost      bool
	released  bool
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

// Recreate creates the sandbox again after an operation answered ErrLost,
// with a fresh workspace and the helper uploaded; the machine is usable
// again and Info names the new sandbox. Restoring the files is the
// runner's, through ImportTar.
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

// attach finds the sandbox by name, starting it when it is stopped, or
// creates it, and makes sure the helper is in it.
func (m *Machine) attach(ctx context.Context) error {
	sb, _, err := m.c.GetSandbox(ctx, m.name)
	created := false
	switch {
	case err == nil:
		sb, err = m.running(ctx, sb)
		if errors.Is(err, errFailed) {
			// A failed sandbox cannot be started again; it is replaced,
			// and the runner restores its files as for a lost one.
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
	m.set(sb, created)
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
	sb, err = m.running(ctx, sb)
	if errors.Is(err, errFailed) || errors.Is(err, errGone) {
		return v1.Sandbox{}, false, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return sb, created, err
}

// set records the running sandbox.
func (m *Machine) set(sb v1.Sandbox, created bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.id = sb.Status.ID
	m.created = created
	m.workspace = sb.Spec.Workspace.Path
	workdir := cmpOr(m.o.Workdir, sb.Spec.Workdir, m.workspace)
	m.info = machine.Info{Kind: machine.KindCella, ID: sb.Status.ID, Workdir: workdir, Environment: sb.Status.Environment,
		OS: m.info.OS, Arch: m.info.Arch}
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

// Errors of a wait for a sandbox to run.
var (
	errFailed = errors.New("the sandbox failed")
	errGone   = errors.New("the sandbox is gone")
)

// running holds until the sandbox runs: a stopped one is started, one on
// its way up or down is read again until it settles, a failed one is
// errFailed and a deleted one errGone.
func (m *Machine) running(ctx context.Context, sb v1.Sandbox) (v1.Sandbox, error) {
	deadline := time.Now().Add(m.o.StartTimeout)
	for {
		switch sb.Status.Phase {
		case "Running":
			return sb, nil
		case "Failed":
			return sb, fmt.Errorf("%w: %s: %s", errFailed, m.name, cmpOr(sb.Status.Reason, "no reason given"))
		case "Stopped":
			started, _, err := m.c.StartSandbox(ctx, sb.Status.ID)
			switch {
			case err == nil:
				sb = started
				continue
			case client.CodeOf(err) == "not_found":
				return sb, errGone
			case client.CodeOf(err) != "phase_conflict":
				// A conflict is a start that raced another; the read
				// below settles it.
				return sb, refused("start the sandbox "+m.name, err)
			}
		}
		if !time.Now().Before(deadline) {
			return sb, fmt.Errorf("%w: the sandbox %s is %s after %s", ErrUnavailable, m.name, sb.Status.Phase, m.o.StartTimeout)
		}
		if err := sleep(ctx, m.o.PollInterval); err != nil {
			return sb, err
		}
		next, _, err := m.c.GetSandbox(ctx, sb.Status.ID)
		if client.CodeOf(err) == "not_found" {
			return sb, errGone
		}
		if err != nil {
			return sb, refused("read the sandbox "+m.name, err)
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
// session, labeled, on the session's Environment, with the egress
// allowlist, the named secrets and the lifecycle.
func (m *Machine) manifest(ctx context.Context) ([]byte, error) {
	hosts := slices.Clone(m.o.Egress)
	for _, s := range m.o.Secrets {
		sec, _, err := m.c.GetSecret(ctx, s.Name)
		if err != nil {
			return nil, refused("read the secret "+s.Name, err)
		}
		hosts = append(hosts, sec.Spec.Scope.Hosts...)
	}
	for i, h := range hosts {
		hosts[i] = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
	}
	slices.Sort(hosts)
	hosts = slices.Compact(hosts)
	hosts = slices.DeleteFunc(hosts, func(h string) bool { return h == "" })
	labels := maps.Clone(m.o.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[LabelSession] = m.o.Session
	if m.o.Agent != "" {
		labels[LabelAgent] = m.o.Agent
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
			Env:         m.o.Env,
			Secrets:     m.o.Secrets,
			Network:     v1.Network{Egress: v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: hosts}},
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

// probe makes the spill directory and reads the sandbox's platform and
// the SHA-256 of the helper already in it, which is absent after a create
// and may be after a start: /tmp does not outlive a stop on every driver.
const probe = `mkdir -p "$2" && uname -s && uname -m && if [ -x "$1" ]; then "$1" sum; fi`

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
		Command: []string{shell, "-c", probe, "sh", m.helper(), m.SpillDir()},
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

// wakeUp brings the sandbox back after a call found it stopped, or found
// no helper in it: it starts the sandbox, holds until it runs, and puts
// the helper back. A sandbox that is gone marks the machine lost.
func (m *Machine) wakeUp(ctx context.Context, id string) error {
	m.wake.Lock()
	defer m.wake.Unlock()
	m.mu.Lock()
	current := m.id
	m.mu.Unlock()
	if current != id {
		// Another call already brought a sandbox back.
		return nil
	}
	sb, _, err := m.c.GetSandbox(ctx, id)
	if client.CodeOf(err) == "not_found" {
		return m.markLost()
	}
	if err != nil {
		return fmt.Errorf("machine: read the sandbox %s: %w", m.name, err)
	}
	sb, err = m.running(ctx, sb)
	if errors.Is(err, errGone) || errors.Is(err, errFailed) {
		return m.markLost()
	}
	if err != nil {
		return err
	}
	return m.ensureHelper(ctx, sb.Status.ID)
}

// markLost records that the sandbox is gone and answers ErrLost; every
// call answers it until Recreate.
func (m *Machine) markLost() error {
	m.mu.Lock()
	m.lost = true
	m.mu.Unlock()
	return fmt.Errorf("%w: %s", ErrLost, m.name)
}

// call runs one operation on the sandbox. A sandbox Cella stopped for
// idleness is started and the operation runs once more; a not_found is
// the sandbox's when the sandbox is gone, which is ErrLost, and the
// path's otherwise. retry is false for an operation whose request body
// cannot be sent twice.
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
		err = f(id)
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
