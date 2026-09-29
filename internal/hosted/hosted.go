// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package hosted builds the harness of a session toposd runs itself
// (spec 016): its agent read back from the session's bundle, its model
// connection to the installation's model URL, and its machine, a Cella
// sandbox created when a tool first acts on it, or the server's own host.
// Every failure of the harness is a runner.SetupError naming its code, so
// the session's turn closes with it instead of the session staying
// running; a sandbox that cannot be created is a machine.OpenError with
// the same code, which answers the call that needed it.
package hosted

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/cella/client"
	cellav1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/pkg/otel"

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
	CodeModelUnavailable       = models.CodeUnavailable
	CodeModelCredentialMissing = models.CodeCredentialMissing
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
	// root, as dialect.Discover read them and under TOPOS_MODELS_URL
	// itself (models.Doors.Under); nil uses it as it is.
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
		model, conn, entry, err := b.connect(ctx, m, overlay)
		return model, &conn, &entry, err
	})
	if err != nil {
		return harness.Config{}, err
	}
	// A session that switched its model starts on the one it switched to
	// (spec 015), and a switch between two turns of the drive connects
	// the next one the same way, with the drive's credentials.
	var name string
	if s.Model != nil {
		name = s.Model.Name
	}
	m, overlay := ac.SessionModel(name)
	model, conn, entry, err := b.connect(ctx, m, overlay)
	if err != nil {
		return harness.Config{}, err
	}
	cfg := harness.Config{
		Model: model, Connection: conn, Entry: entry,
		Connect: func(_ context.Context, name string) (models.Model, models.Connection, models.Entry, error) {
			m, overlay := ac.SessionModel(name)
			model, conn, entry, err := b.connect(ctx, m, overlay)
			if se, ok := errors.AsType[*runner.SetupError](err); ok {
				err = &models.Coded{Code: se.Code, Message: se.Err.Error()}
			}
			return model, conn, entry, err
		},
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
	cfg.Tools = reg
	if cmp.Or(s.Machine.Kind, ac.Machine.Kind) == session.MachineHost {
		m, err := b.o.Machines(ctx, s, ac.Machine)
		if err != nil {
			return harness.Config{}, machineSetup(err)
		}
		cfg.Machine = m
		return cfg, nil
	}
	// A Cella sandbox is created when a tool first acts on it (spec
	// 009), under the drive's context, so a session whose agent never
	// needs one has none.
	cfg.Machine = machine.Defer(ctx, machine.KindCella, func(ctx context.Context) (machine.Machine, error) {
		m, err := b.o.Machines(ctx, s, ac.Machine)
		if err == nil {
			return m, nil
		}
		se, _ := errors.AsType[*runner.SetupError](machineSetup(err))
		return nil, &machine.OpenError{Code: se.Code, Err: se.Err}
	})
	return cfg, nil
}

// machineSetup is a machine that could not be had as the setup error a
// turn closes with: its own code when it names one, machine_unavailable
// otherwise.
func machineSetup(err error) error {
	if _, coded := errors.AsType[*runner.SetupError](err); coded {
		return err
	}
	return setup(CodeMachineUnavailable, err)
}

// connect is the model, the connection and the catalog figures of one
// spec.model: the embedded catalog's, overlaid by the figures a Lux door
// serves for the model, then by the agent's own. A connection to the
// installation's model URL acts with the session's own Lux key when the
// installation mints one (spec 018), and with TOPOS_MODELS_KEY
// otherwise; the session's key never leaves for another base URL.
func (b builder) connect(ctx context.Context, m v1.AgentModel, overlay models.Entry) (models.Model, models.Connection, models.Entry, error) {
	base := cmp.Or(m.BaseURL, b.o.ModelsURL)
	if base == "" {
		return nil, models.Connection{}, models.Entry{}, setup(CodeModelUnavailable, errors.New("the agent names no base URL and TOPOS_MODELS_URL is unset"))
	}
	if m.Credential != "" {
		// A credential the agent names is resolved by the credential
		// custody of spec 018, which this server does not hold.
		return nil, models.Connection{}, models.Entry{}, setup(CodeModelCredentialMissing, fmt.Errorf("the agent names the credential %s, which this server cannot resolve", m.Credential))
	}
	model, credential := b.o.Model, b.o.ModelsKey
	if m.BaseURL == "" {
		key, keyed, ok, err := sessionKey(ctx, b.o.Model)
		if err != nil {
			return nil, models.Connection{}, models.Entry{}, err
		}
		if ok {
			model, credential = keyed, key
		}
	}
	if credential == "" {
		return nil, models.Connection{}, models.Entry{}, setup(CodeModelCredentialMissing, errors.New("the agent names no credential, the server mints no session key, and TOPOS_MODELS_KEY is unset"))
	}
	conn, entry, err := b.resolve(ctx, m, overlay, base, credential)
	if err != nil {
		return nil, models.Connection{}, models.Entry{}, err
	}
	return model, conn, entry, nil
}

// resolve is the connection and the figures of one spec.model at base:
// the embedded catalog's, overlaid by the figures a Lux door serves for
// the model, read with credential, then by the agent's own. An empty
// credential reads the door's list without a key.
func (b builder) resolve(ctx context.Context, m v1.AgentModel, overlay models.Entry, base, credential string) (models.Connection, models.Entry, error) {
	// The family and the dialect pick the door, whose list may name the
	// model's figures; a model known to neither the catalog nor the
	// agent is still asked of its door before it is refused.
	first := b.cat.Overlay(m.Name, overlay)
	spoken := first.Dialect
	if m.BaseURL == "" {
		if spoken == "" && len(b.o.Doors) > 0 {
			// A model neither the catalog nor the agent gives a family goes
			// through Lux's OpenAI door, which serves every model Lux
			// routes, and whose list gives the model's figures; without a
			// door it would never be asked, and refused as unknown.
			spoken = ir.DialectOpenAIChat
		}
		base = b.o.Doors.Door(base, spoken)
	}
	conn := models.Connection{BaseURL: base, Model: m.Name, Credential: credential, Family: first.Family, Dialect: spoken}
	var served models.Entry
	if models.NamesADoor(base) {
		var err error
		if served, err = dialect.Served(ctx, otel.HTTPClient(), conn); err != nil {
			return models.Connection{}, models.Entry{}, setup(CodeModelUnavailable, err)
		}
	}
	entry, err := b.cat.Resolve(m.Name, served, overlay)
	if err != nil {
		return models.Connection{}, models.Entry{}, err
	}
	if entry.Dialect == "" {
		entry.Dialect = spoken
	}
	conn.Family, conn.Dialect = entry.Family, entry.Dialect
	return conn, entry, nil
}

// Figures is the check a server makes of a model a session switches to
// (spec 015): the figures connect would resolve for the spec.model and
// overlay, read at the installation's model URL with TOPOS_MODELS_KEY,
// or without a key when it is unset, since a session's own key is its
// runner's. A model no source gives an input window and an output limit
// is models.CodeUnknown, and a door that does not answer
// models.CodeUnavailable.
func Figures(o Options) (func(ctx context.Context, m v1.AgentModel, overlay models.Entry) (models.Entry, error), error) {
	cat, err := models.Embedded()
	if err != nil {
		return nil, err
	}
	b := builder{o: o, cat: cat}
	return func(ctx context.Context, m v1.AgentModel, overlay models.Entry) (models.Entry, error) {
		base := cmp.Or(m.BaseURL, o.ModelsURL)
		if base == "" {
			return models.Entry{}, &models.Coded{Code: models.CodeUnavailable, Message: "the agent names no base URL and TOPOS_MODELS_URL is unset"}
		}
		credential := ""
		if m.BaseURL == "" {
			credential = o.ModelsKey
		}
		_, entry, err := b.resolve(ctx, m, overlay, base, credential)
		if se, ok := errors.AsType[*runner.SetupError](err); ok {
			err = &models.Coded{Code: se.Code, Message: se.Err.Error()}
		}
		return entry, err
	}, nil
}

// CellaOptions configure the Cella machines of hosted sessions.
type CellaOptions struct {
	// URL is TOPOS_CELLA_URL.
	URL string
	// Token is the installation's bearer, TOPOS_CELLA_TOKEN_FILE's,
	// presented when the server mints no Cella token for the session;
	// nil presents none.
	Token client.TokenSource
	// ModelsURL is the Lux root a sandbox reaches: the root Lux
	// published its doors under when TOPOS_MODELS_URL is a Lux root, and
	// TOPOS_MODELS_URL otherwise. The sandbox's Lux key is scoped to its
	// host, and the sandbox reads it as LUX_URL.
	ModelsURL string
	// OrigoURL is TOPOS_ORIGO_URL, the git host whose token the sandbox's
	// git sends; empty mounts none.
	OrigoURL string
	// OrigoToken is the installation's credential for OrigoURL,
	// TOPOS_ORIGO_TOKEN_FILE's, which the sandbox's git sends when the
	// server mints no git host token for the session; it is read at each
	// sandbox's open, and nil sends none.
	OrigoToken client.TokenSource
	// Labels are TOPOS_CELLA_LABELS, which every sandbox and every Secret
	// the runner applies for it carry beside the session's and the
	// agent's labels.
	Labels map[string]string
	// Log reports a sandbox credential that could not be renewed.
	Log *slog.Logger
	// Helpers are the topos-machine builds by platform.
	Helpers map[string][]byte
	// Dir is where the helper and the spill directory live inside each
	// sandbox, outside its workspace; empty is cella.DefaultDir.
	Dir string
}

// Cella opens each hosted session's machine as a Cella sandbox named
// after the session. Without a URL every session is refused
// machine_unavailable. The runner's calls carry the agent's token for
// the session when the installation mints one, and the installation's
// bearer otherwise; the sandbox's Lux key and git host token are Cella
// Secrets whose placeholders it holds, renewed until the drive ends. An
// installation that mints no git host token puts its own, OrigoToken, in
// the git host's Secret instead, applied again at each open.
func Cella(o CellaOptions) Machines {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return func(ctx context.Context, s session.Session, m v1.Machine) (machine.Machine, error) {
		if o.URL == "" {
			return nil, setup(CodeMachineUnavailable, errors.New("TOPOS_CELLA_URL is unset, so this server has no machines"))
		}
		token, err := cellaToken(ctx, o.Token)
		if err != nil {
			return nil, err
		}
		c, err := client.New(client.Config{URL: o.URL, Token: token, HTTPClient: otel.HTTPClient()})
		if err != nil {
			return nil, setup(CodeMachineUnavailable, err)
		}
		secrets, err := o.sandboxSecrets(ctx, s, c)
		if err != nil {
			return nil, err
		}
		var mounts []cellav1.SecretMount
		var env map[string]string
		for _, sec := range secrets {
			mounts = append(mounts, cellav1.SecretMount{Name: sec.name, Env: sec.env})
			if sec.audience == runner.AudienceLux {
				env = map[string]string{EnvLuxURL: o.ModelsURL}
			}
		}
		ttl := time.Until(s.ExpiresAt)
		mach, err := cella.Open(ctx, cella.Options{
			URL: o.URL, Token: token, Session: s.ID, Agent: s.Agent.Name,
			Environment: cmp.Or(s.Machine.Environment, m.Environment), Image: cmp.Or(s.Machine.Image, m.Image),
			Resources: cellav1.Resources{CPU: cellav1.Quantity(m.Resources.CPU), Memory: cellav1.Quantity(m.Resources.Memory), Disk: cellav1.Quantity(m.Resources.Disk)},
			Egress:    append(slices.Clone(m.Egress), repositoryHosts(s)...), Secrets: mounts, Env: env, Labels: o.Labels, TTL: max(ttl, 0), Helpers: o.Helpers, Dir: o.Dir,
		})
		if err != nil {
			return nil, err
		}
		for _, sec := range secrets {
			if sec.audience == AudienceOrigo {
				if err := gitConfig(ctx, mach, o.OrigoURL); err != nil {
					return nil, errors.Join(setup(CodeMachineUnavailable, err), mach.Release(context.WithoutCancel(ctx), false))
				}
			}
		}
		var minted []*sandboxSecret
		for _, sec := range secrets {
			if !sec.installation {
				minted = append(minted, sec)
			}
		}
		if len(minted) > 0 {
			keep(ctx, c, runner.TokensFrom(ctx), minted, o.Log)
		}
		return mach, nil
	}
}

// repositoryHosts are the git hosts of the session's repositories, which
// the sandbox's egress allowlist includes (spec 009).
func repositoryHosts(s session.Session) []string {
	var out []string
	for _, r := range session.Repositories(s) {
		if u, err := url.Parse(r.URL); err == nil && u.Hostname() != "" {
			out = append(out, u.Hostname())
		}
	}
	return out
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
