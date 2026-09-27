// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package hosted builds the harness of a session toposd runs itself
// (spec 016): its agent read back from the session's bundle, its model
// connection to the installation's model URL, and its machine, a Cella
// sandbox. Every failure is a runner.SetupError naming its code, so the
// session's turn closes with it instead of the session staying running.
package hosted

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"latere.ai/x/cella/client"
	cellav1 "latere.ai/x/cella/manifest/v1"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/cella"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/dialect"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// The setup error codes of a hosted session.
const (
	CodeAgentMissing           = "agent_missing"
	CodeModelUnavailable       = "model_unavailable"
	CodeModelCredentialMissing = "model_credential_missing"
	CodeMachineUnavailable     = machine.CodeUnavailable
)

// Machines opens the machine of a session from its agent's
// spec.machine.
type Machines func(ctx context.Context, s session.Session, m v1.Machine) (machine.Machine, error)

// Options configure the hosted harness.
type Options struct {
	Store session.Store
	// ModelsURL and ModelsKey are TOPOS_MODELS_URL and TOPOS_MODELS_KEY:
	// the model connection of an agent that names no base URL, and the
	// credential of one that names no credential.
	ModelsURL string
	ModelsKey string
	// Doors are the family doors TOPOS_MODELS_URL names when it is a Lux
	// root, as dialect.Discover read them; nil uses it as it is.
	Doors models.Doors
	// Model streams the requests; dialect.Model when nil.
	Model models.Model
	// Machines opens each session's machine.
	Machines Machines
	Clock    func() time.Time
}

// Harness is the runner's Harness function for hosted sessions.
func Harness(o Options) (func(ctx context.Context, s session.Session) (harness.Config, error), error) {
	switch {
	case o.Store == nil:
		return nil, errors.New("hosted: no store")
	case o.Machines == nil:
		return nil, errors.New("hosted: no machines")
	}
	if o.Model == nil {
		o.Model = &dialect.Model{}
	}
	cat, err := models.Embedded()
	if err != nil {
		return nil, err
	}
	b := builder{o: o, cat: cat}
	return b.config, nil
}

type builder struct {
	o   Options
	cat models.Catalog
}

func setup(code string, err error) error { return &runner.SetupError{Code: code, Err: err} }

func (b builder) config(ctx context.Context, s session.Session) (harness.Config, error) {
	r, ok, err := runner.Agent(ctx, b.o.Store, s)
	switch {
	case err != nil:
		return harness.Config{}, setup(CodeAgentMissing, err)
	case !ok:
		return harness.Config{}, setup(CodeAgentMissing, fmt.Errorf("session %s names no agent bundle", s.ID))
	}
	ac, err := r.AgentConfig(func(m v1.AgentModel, overlay models.Entry) (models.Model, *models.Connection, *models.Entry, error) {
		conn, entry, err := b.connect(m, overlay)
		return b.o.Model, &conn, &entry, err
	})
	if err != nil {
		return harness.Config{}, err
	}
	conn, entry, err := b.connect(ac.Model, ac.Overlay)
	if err != nil {
		return harness.Config{}, err
	}
	cfg := harness.Config{
		Model: b.o.Model, Connection: conn, Entry: entry,
		Name: ac.Name, Instructions: ac.Instructions, Policy: ac.Policy, Effort: ac.Effort,
		Subagents: ac.Subagents, MaxDepth: ac.MaxDepth, MaxConcurrent: ac.MaxConcurrent, CompactAt: ac.CompactAt,
		Prompt: prompts.HarnessOptions{Threads: len(ac.Subagents) > 0},
		Clock:  b.o.Clock,
	}
	cfg.Policy.Mode = cmp.Or(cfg.Policy.Mode, harness.ModeConfirm)
	if d, err := time.ParseDuration(s.Limits.TurnTimeout); err == nil {
		cfg.TurnTimeout = d
	}
	reg := tools.NewRegistry()
	for _, t := range tools.Builtins() {
		if slices.Contains(ac.Tools, t.Definition().Name) {
			if err := reg.AddBuiltin(t); err != nil {
				return harness.Config{}, err
			}
		}
	}
	m, err := b.o.Machines(ctx, s, ac.Machine)
	if err != nil {
		if _, coded := errors.AsType[*runner.SetupError](err); coded {
			return harness.Config{}, err
		}
		return harness.Config{}, setup(CodeMachineUnavailable, err)
	}
	cfg.Machine, cfg.Tools = m, reg
	return cfg, nil
}

// connect is the connection and the catalog figures of one spec.model.
func (b builder) connect(m v1.AgentModel, overlay models.Entry) (models.Connection, models.Entry, error) {
	base := cmp.Or(m.BaseURL, b.o.ModelsURL)
	if base == "" {
		return models.Connection{}, models.Entry{}, setup(CodeModelUnavailable, errors.New("the agent names no base URL and TOPOS_MODELS_URL is unset"))
	}
	if m.Credential != "" {
		// A credential the agent names is resolved by the credential
		// custody of spec 018, which this server does not hold.
		return models.Connection{}, models.Entry{}, setup(CodeModelCredentialMissing, fmt.Errorf("the agent names the credential %s, which this server cannot resolve", m.Credential))
	}
	if b.o.ModelsKey == "" {
		return models.Connection{}, models.Entry{}, setup(CodeModelCredentialMissing, errors.New("the agent names no credential and TOPOS_MODELS_KEY is unset"))
	}
	entry, err := b.cat.Resolve(m.Name, overlay)
	if err != nil {
		return models.Connection{}, models.Entry{}, err
	}
	if m.BaseURL == "" {
		base = b.o.Doors.Door(base, entry.Dialect)
	}
	conn := models.Connection{BaseURL: base, Model: m.Name, Credential: b.o.ModelsKey, Family: entry.Family, Dialect: entry.Dialect}
	return conn, entry, nil
}

// CellaOptions configure the Cella machines of hosted sessions.
type CellaOptions struct {
	// URL is TOPOS_CELLA_URL.
	URL string
	// Token is the bearer toposd presents to Cella.
	Token client.TokenSource
	// Helpers are the topos-machine builds by platform.
	Helpers map[string][]byte
	// Dir is where the helper and the spill directory live inside each
	// sandbox, outside its workspace; empty is cella.DefaultDir.
	Dir string
}

// Cella opens each hosted session's machine as a Cella sandbox named
// after the session. Without a URL every session is refused
// machine_unavailable.
func Cella(o CellaOptions) Machines {
	return func(ctx context.Context, s session.Session, m v1.Machine) (machine.Machine, error) {
		if o.URL == "" {
			return nil, setup(CodeMachineUnavailable, errors.New("TOPOS_CELLA_URL is unset, so this server has no machines"))
		}
		ttl := time.Until(s.ExpiresAt)
		return cella.Open(ctx, cella.Options{
			URL: o.URL, Token: o.Token, Session: s.ID, Agent: s.Agent.Name,
			Environment: cmp.Or(s.Machine.Environment, m.Environment), Image: cmp.Or(s.Machine.Image, m.Image),
			Resources: cellav1.Resources{CPU: cellav1.Quantity(m.Resources.CPU), Memory: cellav1.Quantity(m.Resources.Memory), Disk: cellav1.Quantity(m.Resources.Disk)},
			Egress:    m.Egress, TTL: max(ttl, 0), Helpers: o.Helpers, Dir: o.Dir,
		})
	}
}

// ReadHelpers reads the topos-machine builds under dir, each named
// topos-machine-<os>-<arch> and keyed "<os>/<arch>". No build at all is
// an error.
func ReadHelpers(dir string) (map[string][]byte, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "topos-machine-*-*"))
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, p := range paths {
		goos, arch, ok := strings.Cut(strings.TrimPrefix(filepath.Base(p), "topos-machine-"), "-")
		if !ok || goos == "" || arch == "" {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		out[goos+"/"+arch] = b
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no topos-machine build under %s", dir)
	}
	return out, nil
}
