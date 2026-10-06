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
	"latere.ai/x/topos/internal/publish"
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
	// CodeSearchUnavailable is a search service whose URL the client
	// refuses, which configuration checks first (spec 047).
	CodeSearchUnavailable = "search_unavailable"
)

// DoorSettle is how long a runner reads a door's model list again when
// the door answers with a list of other models and not the one it
// connects (spec 051). A door lists the models the presented key may use, and a
// gateway that runs several replicas applies a change of a key's models
// on each replica within a window of its own: an authorizer that has
// just moved a session to another model widened the session's key
// moments before the runner connects the model, and a replica that has
// not read the change yet lists the key's earlier models. Lux's replicas
// read a key's change within a second of it. A model the list still does
// not name once DoorSettle has passed is connected as it was before: by
// the embedded catalog's figures, or refused as unknown.
const DoorSettle = 3 * time.Second

// settleEvery is how soon a door's list is read again within DoorSettle.
const settleEvery = 250 * time.Millisecond

// Machines opens the machine of a session from its agent's
// spec.machine. A Cella machine's m carries the session's network in
// place of the agent's egress (spec 052): its mode as EgressMode and,
// under allowlist, every host it reaches beside its secrets' as Egress.
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
	// SearchURL and SearchKey are TOPOS_SEARCH_URL and TOPOS_SEARCH_KEY:
	// the search service of an agent that names web_search, and the
	// bearer sent to it when the installation mints no session keys
	// (spec 047). An empty URL offers the tool with no service.
	SearchURL string
	SearchKey string
	// Publish is the installation's app host, which the publish tool
	// reaches for a session whose agent names it (spec 043); unconfigured,
	// no session is offered the tool.
	Publish publish.Options
	Clock   func() time.Time
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
	// web_search is a built-in an agent holds only by naming it (spec
	// 047), with the installation's search service.
	if slices.Contains(ac.Tools, tools.NameWebSearch) {
		s, err := b.searcher(ctx)
		if err != nil {
			return harness.Config{}, err
		}
		if err := reg.AddBuiltin(tools.WebSearch(s)); err != nil {
			return harness.Config{}, err
		}
	}
	cfg.Tools = reg
	// The question tool is the harness's own, offered when the agent names
	// it (spec 039).
	cfg.Question = slices.Contains(ac.Tools, harness.ToolQuestion)
	onHost := cmp.Or(s.Machine.Kind, ac.Machine.Kind) == session.MachineHost
	// The publish tool is offered to a session in a sandbox whose agent
	// names it, on an installation with an app host (spec 043); it reaches
	// the host with the drive's tokens.
	if slices.Contains(ac.Tools, publish.Name) && b.o.Publish.Configured() && !onHost {
		if err := reg.Add(publish.New(b.o.Publish, s, runner.TokensFrom(ctx))); err != nil {
			return harness.Config{}, err
		}
	}
	if onHost {
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
		spec, err := b.withNetwork(ctx, s, ac.Machine)
		if err != nil {
			return nil, &machine.OpenError{Code: CodeMachineUnavailable, Err: err}
		}
		m, err := b.o.Machines(ctx, s, spec)
		if err == nil {
			return m, nil
		}
		se, _ := errors.AsType[*runner.SetupError](machineSetup(err))
		return nil, &machine.OpenError{Code: se.Code, Err: se.Err}
	})
	return cfg, nil
}

// withNetwork is the agent's spec.machine with its egress replaced by the
// session's network as the store holds it when the machine opens (spec
// 052): the header's base network, or the agent's, with the hosts a
// person allowed, the agent's and the repositories' joined under
// allowlist. A sandbox created or found by a runner that claims the
// session later takes the same network from the same log.
func (b builder) withNetwork(ctx context.Context, s session.Session, m v1.Machine) (v1.Machine, error) {
	cur, err := b.o.Store.Get(ctx, s.ID)
	if err != nil {
		return v1.Machine{}, fmt.Errorf("read the session's network: %w", err)
	}
	evs, err := b.o.Store.Events(ctx, s.ID, 1, 0)
	if err != nil {
		return v1.Machine{}, fmt.Errorf("read the session's network: %w", err)
	}
	n := harness.EffectiveNetwork(cur, evs, m.Mode(), m.Egress)
	m.EgressMode, m.Egress = n.Mode, n.Hosts
	return m, nil
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
// otherwise. The session's key goes to the installation's model URL and
// its search URL (spec 047), both the operator's settings, and never to
// a base URL an agent names.
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

// resolve is the connection and the figures of one spec.model at base,
// its route's door read with credential, again for DoorSettle while the
// door lists other models alone.
func (b builder) resolve(ctx context.Context, m v1.AgentModel, overlay models.Entry, base, credential string) (models.Connection, models.Entry, error) {
	conn := b.route(m, overlay, base)
	conn.Credential = credential
	entry, err := b.figures(ctx, conn, m, overlay, DoorSettle)
	if err != nil {
		return models.Connection{}, models.Entry{}, err
	}
	conn.Family, conn.Dialect = entry.Family, entry.Dialect
	return conn, entry, nil
}

// route is the connection of one spec.model at base, without its
// credential: the family and the dialect the embedded catalog and the
// agent give pick the door when base is the installation's model URL,
// and a model neither gives a family goes through Lux's OpenAI door,
// which serves every model Lux routes and whose list gives the model's
// figures; without a door it would never be asked, and refused as
// unknown. An agent's own base URL is used as it is.
func (b builder) route(m v1.AgentModel, overlay models.Entry, base string) models.Connection {
	first := b.cat.Overlay(m.Name, overlay)
	spoken := first.Dialect
	if m.BaseURL == "" {
		if spoken == "" && len(b.o.Doors) > 0 {
			spoken = ir.DialectOpenAIChat
		}
		base = b.o.Doors.Door(base, spoken)
	}
	return models.Connection{BaseURL: base, Model: m.Name, Family: first.Family, Dialect: spoken}
}

// figures are the model's figures on conn: the embedded catalog's,
// overlaid by those a Lux door serves for the model, read with the
// connection's credential, then by the agent's own. A model no source
// gives an input window and an output limit is model_unknown. A door
// that answers a list without the model is read again for settle, as
// DoorSettle says; the figures are those of the last read.
func (b builder) figures(ctx context.Context, conn models.Connection, m v1.AgentModel, overlay models.Entry, settle time.Duration) (models.Entry, error) {
	var served models.Entry
	if models.NamesADoor(conn.BaseURL) {
		var err error
		if served, err = servedSettled(ctx, conn, settle); err != nil {
			return models.Entry{}, setup(CodeModelUnavailable, err)
		}
	}
	entry, err := b.cat.Resolve(m.Name, served, overlay)
	if err != nil {
		return models.Entry{}, err
	}
	if entry.Dialect == "" {
		entry.Dialect = conn.Dialect
	}
	return entry, nil
}

// servedSettled is the figures the door at conn serves for its model,
// read again every settleEvery while the door answers a list of other
// models alone, until settle has passed. A door that answers no list or a
// list of no model, one that names the model, and an error end the reads
// at once.
func servedSettled(ctx context.Context, conn models.Connection, settle time.Duration) (models.Entry, error) {
	deadline := time.Now().Add(settle)
	for {
		served, listed, err := dialect.Listed(ctx, otel.HTTPClient(), conn)
		if err != nil || !listed || served.Name != "" || !time.Now().Add(settleEvery).Before(deadline) {
			return served, err
		}
		wait := time.NewTimer(settleEvery)
		select {
		case <-ctx.Done():
			wait.Stop()
			return models.Entry{}, ctx.Err()
		case <-wait.C:
		}
	}
}

// Runnable is the one answer to whether this installation runs a
// spec.model (spec 007), which the server asks of a session's model at
// its create and at a switch (spec 015): the model resolves by the rule
// its runner connects it with, route and figures. The door's list is
// read with TOPOS_MODELS_KEY, as the runner reads it without a session
// key. An installation without that key has each runner read its
// doors with the session's own Lux key, which the server does not hold
// and without which a door lists nothing, so a model that goes through
// a Lux door there runs, and its figures are the runner's to read at the
// turn. A model no source gives an input window and an output limit is
// models.CodeUnknown, and a door that does not answer, or no model URL,
// models.CodeUnavailable.
func Runnable(o Options) (func(ctx context.Context, m v1.AgentModel, overlay models.Entry) error, error) {
	cat, err := models.Embedded()
	if err != nil {
		return nil, err
	}
	b := builder{o: o, cat: cat}
	return func(ctx context.Context, m v1.AgentModel, overlay models.Entry) error {
		base := cmp.Or(m.BaseURL, o.ModelsURL)
		if base == "" {
			return &models.Coded{Code: models.CodeUnavailable, Message: "the agent names no base URL and TOPOS_MODELS_URL is unset"}
		}
		conn := b.route(m, overlay, base)
		if m.BaseURL == "" {
			if o.ModelsKey == "" && models.NamesADoor(conn.BaseURL) {
				return nil
			}
			conn.Credential = o.ModelsKey
		}
		// The installation's own key is one no authorizer's move
		// widens, so its door is read once.
		_, err := b.figures(ctx, conn, m, overlay, 0)
		if se, ok := errors.AsType[*runner.SetupError](err); ok {
			err = &models.Coded{Code: se.Code, Message: se.Err.Error()}
		}
		return err
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
			Resources:  cellav1.Resources{CPU: cellav1.Quantity(m.Resources.CPU), Memory: cellav1.Quantity(m.Resources.Memory), Disk: cellav1.Quantity(m.Resources.Disk)},
			EgressMode: m.EgressMode, Egress: session.JoinHosts(m.Egress, harness.RepositoryHosts(s)),
			Secrets: mounts, Env: env, Labels: o.Labels, TTL: max(ttl, 0), Helpers: o.Helpers, Dir: o.Dir,
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
		stop := func() {}
		if len(minted) > 0 {
			stop = keep(ctx, c, runner.TokensFrom(ctx), minted, o.Log)
		}
		if len(secrets) == 0 {
			return mach, nil
		}
		return &sessionMachine{Machine: mach, c: c, secrets: secrets, stop: stop}, nil
	}
}

// sessionMachine is a hosted session's sandbox with the Secrets applied
// for it, which are the session's as the sandbox is (spec 018): at the
// session's end their renewal stops, the sandbox is deleted, and then
// the Secrets, since a Cella Secret has no lifetime of its own and would
// otherwise outlive every session that made one.
type sessionMachine struct {
	*cella.Machine
	c       *client.Client
	secrets []*sandboxSecret
	stop    func()
}

// Release at the session's end deletes the sandbox before its Secrets, so
// no sandbox is left mounting a Secret that is gone. A delete Cella could
// not do leaves the machine to be released again, and the next release
// deletes what is left.
func (m *sessionMachine) Release(ctx context.Context, end bool) error {
	if !end {
		return m.Machine.Release(ctx, false)
	}
	m.stop()
	if err := m.Machine.Release(ctx, true); err != nil {
		return err
	}
	return remove(ctx, m.c, m.secrets)
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
