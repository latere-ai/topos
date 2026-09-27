// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package toposcli is the topos command (spec 024): it runs a local
// session in print mode in the working directory, over the directory
// store, and continues it after a confirmation. It has no interactive
// terminal.
package toposcli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"
	"latere.ai/x/pkg/otel"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/internal/config"
	"latere.ai/x/topos/internal/version"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/host"
	"latere.ai/x/topos/manifest"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/dialect"
	"latere.ai/x/topos/models/scripted"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/dir"
)

// Exit codes of spec 024.
const (
	ExitOK          = 0
	ExitError       = 1
	ExitUsage       = 2
	ExitWaiting     = 3
	ExitLimit       = 4
	ExitInterrupted = 5
)

// Env is the command's view of its process: variables, streams, and the
// working directory.
type Env struct {
	Getenv func(string) string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Dir    string
	// Model overrides the HTTP model, for tests.
	Model models.Model
	// Signals delivers interrupts; nil subscribes to os.Interrupt.
	Signals <-chan os.Signal
}

// Run runs the command and returns its exit code.
func Run(ctx context.Context, args []string, env Env) int {
	c := &cli{Env: env, stdout: &console{w: env.Stdout}, stderr: &console{w: env.Stderr}}
	code := c.run(ctx, args)
	if code == ExitOK && c.stdout.err != nil {
		return ExitError
	}
	return code
}

// cli is one invocation: the environment and its two output streams.
type cli struct {
	Env
	stdout, stderr *console
}

// console is one output stream. It keeps the first write error, so an
// invocation whose output was lost exits 1 instead of reporting success.
type console struct {
	w   io.Writer
	mu  sync.Mutex
	err error
}

func (c *console) printf(format string, a ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		_, c.err = fmt.Fprintf(c.w, format, a...)
	}
}

func (c *console) println(a ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		_, c.err = fmt.Fprintln(c.w, a...)
	}
}

func (c *cli) run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		usage(c.stderr)
		return ExitUsage
	}
	switch args[0] {
	case "-version", "--version", "version":
		c.stdout.println(version.String("topos"))
		return ExitOK
	case "run":
		return runCmd(ctx, args[1:], c)
	case "confirm":
		return confirmCmd(ctx, args[1:], c)
	case "rewind":
		return rewindCmd(ctx, args[1:], c)
	case "-h", "--help", "help":
		usage(c.stdout)
		return ExitOK
	}
	c.stderr.printf("topos: unknown command %q\n", args[0])
	usage(c.stderr)
	return ExitUsage
}

func usage(w *console) {
	w.printf("%s", `usage:
  topos run [flags] [<prompt>]        run a turn of a local session in the working directory
                                      (--agent <file>, --model, --mode, --max-cost, --dir, --output, --session)
  topos confirm <session> <tool_use_id> allow|deny [--note <text>] [--remember <pattern>]
  topos rewind <session> <turn>       restore the working directory to the end of a turn
  topos version
`)
}

// runOptions are the flags of run and confirm.
type runOptions struct {
	session string
	agent   string
	model   string
	mode    string
	maxCost float64
	dir     string
	output  string
}

func (o *runOptions) flags(fs *flag.FlagSet) {
	fs.StringVar(&o.session, "session", "", "continue this session")
	fs.StringVar(&o.model, "model", "", "the model to run, by catalog name; replaces the agent's")
	fs.StringVar(&o.mode, "mode", "", "plan, confirm or progressive; empty is the agent's, else confirm")
	fs.Float64Var(&o.maxCost, "max-cost", 0, "the session's budget in USD; 0 is none")
	fs.StringVar(&o.dir, "dir", "", "the working directory; empty is the current one")
	fs.StringVar(&o.output, "output", "text", "text, json or stream-json")
}

func (o runOptions) validate() error {
	switch harness.Mode(o.mode) {
	case "", harness.ModePlan, harness.ModeConfirm, harness.ModeProgressive:
	default:
		return fmt.Errorf("--mode %q is not plan, confirm or progressive", o.mode)
	}
	switch o.output {
	case "text", "json", "stream-json":
	default:
		return fmt.Errorf("--output %q is not text, json or stream-json", o.output)
	}
	if o.maxCost < 0 || math.IsNaN(o.maxCost) || math.IsInf(o.maxCost, 0) {
		return errors.New("--max-cost is a non-negative amount in USD")
	}
	return nil
}

func runCmd(ctx context.Context, args []string, env *cli) int {
	fs := flag.NewFlagSet("topos run", flag.ContinueOnError)
	fs.SetOutput(env.stderr.w)
	var o runOptions
	o.flags(fs)
	fs.StringVar(&o.agent, "agent", "", "the agent manifest to run; default $XDG_CONFIG_HOME/topos/agent.yaml when present")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if err := o.validate(); err != nil {
		env.stderr.println("topos:", err)
		return ExitUsage
	}
	if env.Getenv("TOPOS_URL") != "" {
		env.stderr.println("topos: server sessions are not built yet; unset TOPOS_URL to run a local session")
		return ExitUsage
	}
	if o.session != "" && o.agent != "" {
		env.stderr.println("topos: --agent starts a new session; a session keeps the agent it was created with")
		return ExitUsage
	}
	prompt := strings.Join(fs.Args(), " ")
	if prompt == "" {
		b, err := io.ReadAll(env.Stdin)
		if err != nil {
			env.stderr.println("topos: read the prompt:", err)
			return ExitError
		}
		prompt = strings.TrimSpace(string(b))
	}
	if prompt == "" {
		env.stderr.println("topos: no prompt: pass it as an argument or on stdin")
		return ExitUsage
	}
	var agent *manifest.Resolved
	if o.session == "" {
		a, err := loadAgent(ctx, env, o.agent)
		if err != nil {
			return report(env, err)
		}
		agent = a
	}
	l, err := openLocal(env, o)
	if err != nil {
		return report(env, err)
	}
	id := o.session
	if id == "" {
		s, err := l.create(ctx, o, agent)
		if err != nil {
			return report(env, err)
		}
		id = s.ID
	}
	msg, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: l.person, Content: []lux.Block{{Type: ir.BlockText, Text: prompt}}}, time.Now())
	if err != nil {
		return report(env, err)
	}
	if err := l.append(ctx, id, msg); err != nil {
		return report(env, err)
	}
	return l.drive(ctx, id, o, env)
}

func confirmCmd(ctx context.Context, args []string, env *cli) int {
	fs := flag.NewFlagSet("topos confirm", flag.ContinueOnError)
	fs.SetOutput(env.stderr.w)
	var o runOptions
	o.flags(fs)
	note := fs.String("note", "", "a note for the model")
	remember := fs.String("remember", "", "allow calls matching this pattern for the rest of the session")
	pos, flagArgs := splitPositional(args, 3)
	if err := fs.Parse(flagArgs); err != nil {
		return ExitUsage
	}
	if len(pos) != 3 || (pos[2] != session.DecisionAllow && pos[2] != session.DecisionDeny) {
		env.stderr.println("topos: confirm takes <session> <tool_use_id> allow|deny")
		return ExitUsage
	}
	if err := o.validate(); err != nil {
		env.stderr.println("topos:", err)
		return ExitUsage
	}
	l, err := openLocal(env, o)
	if err != nil {
		return report(env, err)
	}
	c, err := session.NewEvent(session.TypeUserToolConfirmation, session.UserToolConfirmation{
		Sender: l.person, ToolUseID: pos[1], Decision: pos[2], Note: *note, Remember: *remember,
	}, time.Now())
	if err != nil {
		return report(env, err)
	}
	if err := l.append(ctx, pos[0], c); err != nil {
		return report(env, err)
	}
	return l.drive(ctx, pos[0], o, env)
}

func rewindCmd(ctx context.Context, args []string, env *cli) int {
	if len(args) != 2 {
		env.stderr.println("topos: rewind takes <session> <turn>")
		return ExitUsage
	}
	turn, err := strconv.Atoi(args[1])
	if err != nil || turn < 1 {
		env.stderr.println("topos: the turn is a number from 1")
		return ExitUsage
	}
	l, err := openLocal(env, runOptions{})
	if err != nil {
		return report(env, err)
	}
	r, err := l.runner(runOptions{})
	if err != nil {
		return report(env, err)
	}
	rw, err := r.Rewind(ctx, args[0], turn, l.person)
	if err != nil {
		return report(env, err)
	}
	env.stdout.printf("restored the working directory to the end of turn %d (%s)\n", rw.ToTurn, rw.Checkpoint.Commit)
	return ExitOK
}

// runner is the local runner over the store.
func (l *local) runner(o runOptions) (*runner.Runner, error) {
	return runner.New(runner.Options{
		Store: l.store, Harness: l.config(o), ID: "topos-" + fmt.Sprint(os.Getpid()), Kind: runner.KindLocal,
		CheckpointDir: filepath.Join(l.dataDir, "checkpoints"),
	})
}

// splitPositional takes the first n arguments that are not flags as
// positional, so flags may follow them.
func splitPositional(args []string, n int) ([]string, []string) {
	var pos, rest []string
	for i := range args {
		a := args[i]
		if len(pos) < n && !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
			continue
		}
		rest = append(rest, args[i:]...)
		break
	}
	return pos, rest
}

// local is a local installation: the directory store and the person.
type local struct {
	store   *dir.Store
	dataDir string
	person  session.Sender
	workdir string
	model   models.Model
	// scripted plays scripted: connections, for tests (spec 026).
	scripted *scripted.Model
	getenv   func(string) string

	// doors are the family doors of TOPOS_MODELS_URL when it names a Lux
	// root, read once per invocation.
	doorsMu    sync.Mutex
	doors      models.Doors
	discovered bool
}

func openLocal(env *cli, o runOptions) (*local, error) {
	dataDir, err := config.DataDir(env.Getenv)
	if err != nil {
		return nil, err
	}
	st, err := dir.Open(dataDir)
	if err != nil {
		return nil, err
	}
	workdir := o.dir
	if workdir == "" {
		workdir = env.Dir
	}
	workdir, err = filepath.Abs(workdir)
	if err != nil {
		return nil, err
	}
	user := env.Getenv("USER")
	if user == "" {
		user = "local"
	}
	m := env.Model
	if m == nil {
		m = &dialect.Model{}
	}
	return &local{
		store: st, dataDir: dataDir, workdir: workdir, model: m, scripted: &scripted.Model{}, getenv: env.Getenv,
		person: session.Sender{Subject: "local:" + user, Name: user, Kind: session.SenderPerson},
	}, nil
}

// create creates the session: of the resolved agent when there is one,
// otherwise of the built-in agent. The agent's resolved spec and its
// bundle are blobs of the session, so every later invocation that
// continues it, and a runner elsewhere, runs the same agent.
func (l *local) create(ctx context.Context, o runOptions, agent *manifest.Resolved) (session.Session, error) {
	s := session.New(session.AgentRef{ID: builtinAgent, Name: "topos", Version: 1}, l.person, session.RunnerExternal, session.Machine{Kind: machine.KindHost, Workdir: l.workdir}, time.Now())
	s.Writer = &session.Writer{Kind: session.RunnerExternal, Subject: l.person.Subject, Since: s.CreatedAt}
	if o.maxCost > 0 {
		micro := int64(math.Ceil(o.maxCost * 1e6))
		s.Budget.MaxCostUSDMicro = &micro
	}
	s.Metadata = map[string]string{"model": o.model, "mode": o.mode}
	var blobs map[session.Digest][]byte
	if agent != nil {
		c, err := agent.AgentConfig(nil)
		if err != nil {
			return session.Session{}, err
		}
		if s.Budget.MaxCostUSDMicro == nil {
			s.Budget.MaxCostUSDMicro = c.MaxCostUSDMicro
		}
		lim := agent.Agent.Spec.Limits
		s.Limits = session.Limits{TurnTimeout: lim.TurnTimeout, MaxAge: lim.MaxAge}
		s.ExpiresAt = s.CreatedAt.Add(c.MaxAge)
		if s.Agent, blobs, err = runner.AgentRef(*agent); err != nil {
			return session.Session{}, err
		}
	}
	if err := l.claim(ctx, &s); err != nil {
		return session.Session{}, err
	}
	if err := l.store.Create(ctx, s, blobs); err != nil {
		return session.Session{}, err
	}
	return s, nil
}

// claim picks the session's working directory: the checkout it started
// in, or a worktree of its own when another session that has not ended
// writes that checkout (spec 009).
func (l *local) claim(ctx context.Context, s *session.Session) error {
	c, err := host.Claim(ctx, host.ClaimOptions{
		Dir: s.Machine.Workdir, WorktreeDir: filepath.Join(l.dataDir, "worktrees"), Agent: s.Agent.Name, Session: s.ID,
		Active: func(ctx context.Context, id string) (bool, error) {
			other, err := l.store.Get(ctx, id)
			if errors.Is(err, session.ErrNotFound) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			return other.Status != session.StatusEnded, nil
		},
	})
	if err != nil {
		return err
	}
	s.Machine.Workdir = c.Workdir
	return nil
}

// ConfigDir is $XDG_CONFIG_HOME/topos, or .config/topos under the home
// directory of config.Home, or empty when none of the variables is set.
func ConfigDir(getenv func(string) string) string {
	if d := getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "topos")
	}
	if h := config.Home(getenv); h != "" {
		return filepath.Join(h, ".config", "topos")
	}
	return ""
}

// loadAgent resolves the agent a new session runs: the --agent file, or
// agent.yaml in the configuration directory when it exists, or none,
// which is the built-in agent. The first Agent document of the file is
// the one that runs; the others are the agents it references. A local
// run knows no stored objects, so every reference names a document of
// the same file.
func loadAgent(ctx context.Context, env *cli, file string) (*manifest.Resolved, error) {
	if file == "" {
		dir := ConfigDir(env.Getenv)
		if dir == "" {
			return nil, nil
		}
		file = filepath.Join(dir, "agent.yaml")
		if _, err := os.Stat(file); errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
	} else if !filepath.IsAbs(file) {
		file = filepath.Join(env.Dir, file)
	}
	rs, err := resolveFile(ctx, env, "--agent", file)
	if err != nil {
		return nil, err
	}
	var agent *manifest.Resolved
	for i := range rs {
		if rs[i].Agent != nil && (agent == nil || rs[i].Doc < agent.Doc) {
			agent = &rs[i]
		}
	}
	if agent == nil {
		return nil, &errUsage{file + ": no Agent document to run"}
	}
	if paths := localUnsupported(agent); len(paths) > 0 {
		return nil, &errUsage{fmt.Sprintf("%s: a local run does not apply %s yet", file, strings.Join(paths, ", "))}
	}
	return agent, nil
}

// resolveFile resolves the manifest file the way the topos command
// reads every manifest: its documents through manifest.Resolve, with the
// file's directory as the only file system instructionsFile reads from
// and no stored object to reference. flag names the option the file came
// from in a usage error.
func resolveFile(ctx context.Context, env *cli, flag, file string) ([]manifest.Resolved, error) {
	body, err := os.ReadFile(file)
	if err != nil {
		return nil, &errUsage{flag + ": " + err.Error()}
	}
	root, err := os.OpenRoot(filepath.Dir(file))
	if err != nil {
		return nil, &errUsage{flag + ": " + err.Error()}
	}
	defer func() {
		if err := root.Close(); err != nil {
			env.stderr.println("topos: close the manifest's directory:", err)
		}
	}()
	rs, err := manifest.Resolve(ctx, body, manifest.Options{Files: root.FS()})
	if err != nil {
		return nil, &errUsage{fmt.Sprintf("%s: %v", file, err)}
	}
	return rs, nil
}

// localUnsupported lists the fields of an agent and its subagents that
// a local run cannot honor. Each is refused rather than ignored: a hook
// or a client tool left out would change what the agent may do.
func localUnsupported(r *manifest.Resolved) []string {
	var out []string
	var walk func(at string, s v1.AgentSpec)
	walk = func(at string, s v1.AgentSpec) {
		for i, t := range s.Tools {
			if t.Client || t.OutputLimit != 0 {
				out = append(out, fmt.Sprintf("%s.tools[%d]", at, i))
			}
		}
		for _, f := range []struct {
			name string
			set  bool
		}{
			{"hooks", len(s.Hooks) > 0}, {"advisor", s.Advisor != nil}, {"skills", len(s.Skills) > 0},
			{"mcpServers", len(s.MCPServers) > 0}, {"memoryStores", len(s.MemoryStores) > 0},
			{"connections", len(s.Connections) > 0}, {"repositories", len(s.Repositories) > 0},
			{"machine.kind cella", s.Machine.Kind == v1.MachineCella},
		} {
			if f.set {
				out = append(out, at+"."+f.name)
			}
		}
		for i, sub := range s.Subagents {
			if sub.Spec != nil {
				walk(fmt.Sprintf("%s.subagents[%d].spec", at, i), *sub.Spec)
			}
		}
	}
	walk("spec", r.Agent.Spec)
	refs := make([]string, 0, len(r.Pinned))
	for ref := range r.Pinned {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	for _, ref := range refs {
		walk(r.Pinned[ref].Metadata.Name+".spec", r.Pinned[ref].Spec)
	}
	return out
}

// builtinAgent is the agent a run with no manifest runs.
const builtinAgent = "agent_00000000000000000000000000"

// append adds one event after the session's last sequence. A runner may
// append between the read and the write; the event is then stamped
// again after the new last sequence.
func (l *local) append(ctx context.Context, id string, e session.Event) error {
	for attempt := 0; ; attempt++ {
		s, err := l.store.Get(ctx, id)
		if err != nil {
			return err
		}
		evs := []session.Event{e}
		session.Stamp(id, s.LastSeq, evs)
		_, err = l.store.Append(ctx, id, s.LastSeq, evs)
		if !errors.Is(err, session.ErrSequenceConflict) || attempt == 9 {
			return err
		}
	}
}

// scriptedEntry is the figures of a scripted model, which no catalog
// names: windows large enough for any test, and no price.
func scriptedEntry(name string) models.Entry {
	return models.Entry{Name: name, Family: models.FamilyOther, InputWindow: 200_000, MaxOutputTokens: 32_000}
}

// errUsage is a usage error found after the flags parsed.
type errUsage struct{ msg string }

func (e *errUsage) Error() string { return e.msg }

// connect is the model a name runs on: the connection to the agent's
// base URL or TOPOS_MODELS_URL with TOPOS_MODELS_KEY, and the figures
// of the catalog overlaid by those a Lux door serves for the model and
// then by the agent's own, or of the scripted model.
func (l *local) connect(ctx context.Context, m v1.AgentModel, overlay *models.Entry) (models.Model, models.Connection, models.Entry, error) {
	base := m.BaseURL
	if base == "" {
		base = l.getenv("TOPOS_MODELS_URL")
	}
	if base == "" {
		return nil, models.Connection{}, models.Entry{}, &errUsage{"no model connection: set TOPOS_MODELS_URL"}
	}
	conn := models.Connection{BaseURL: base, Model: m.Name, Credential: l.getenv("TOPOS_MODELS_KEY")}
	if conn.Scripted() {
		entry := scriptedEntry(m.Name)
		conn.Family, conn.Dialect = entry.Family, entry.Dialect
		return l.scripted, conn, entry, nil
	}
	cat, err := models.Embedded()
	if err != nil {
		return nil, models.Connection{}, models.Entry{}, err
	}
	var agent []models.Entry
	if overlay != nil {
		agent = append(agent, *overlay)
	}
	// The family and the dialect pick the door, whose list may name the
	// model's figures; a model known to neither the catalog nor the
	// agent is still asked of its door before it is refused.
	first := cat.Overlay(m.Name, agent...)
	conn.Family, conn.Dialect = first.Family, first.Dialect
	if m.BaseURL == "" {
		doors, err := l.modelDoors(ctx, base)
		if err != nil {
			return nil, models.Connection{}, models.Entry{}, err
		}
		conn.BaseURL = doors.Door(base, conn.EffectiveDialect())
	}
	var served models.Entry
	if models.NamesADoor(conn.BaseURL) {
		if served, err = dialect.Served(ctx, otel.HTTPClient(), conn); err != nil {
			return nil, models.Connection{}, models.Entry{}, err
		}
	}
	entry, err := cat.Resolve(m.Name, append([]models.Entry{served}, agent...)...)
	if err != nil {
		return nil, models.Connection{}, models.Entry{}, err
	}
	conn.Family, conn.Dialect = entry.Family, entry.Dialect
	return l.model, conn, entry, nil
}

// modelDoors are the family doors of TOPOS_MODELS_URL, asked of it the
// first time a connection needs them: a Lux root names them, and any
// other base names none and is used as it is.
func (l *local) modelDoors(ctx context.Context, base string) (models.Doors, error) {
	l.doorsMu.Lock()
	defer l.doorsMu.Unlock()
	if !l.discovered {
		doors, err := dialect.Discover(ctx, otel.HTTPClient(), base)
		if err != nil {
			return nil, fmt.Errorf("TOPOS_MODELS_URL: %w", err)
		}
		l.doors, l.discovered = doors, true
	}
	return l.doors, nil
}

// agentConfig is the harness pieces of the session's agent, read back
// from its bundle blob; nil for the built-in agent.
func (l *local) agentConfig(ctx context.Context, s session.Session) (*manifest.AgentConfig, error) {
	r, ok, err := runner.Agent(ctx, l.store, s)
	if err != nil || !ok {
		return nil, err
	}
	c, err := r.AgentConfig(func(m v1.AgentModel, overlay models.Entry) (models.Model, *models.Connection, *models.Entry, error) {
		model, conn, entry, err := l.connect(ctx, m, &overlay)
		return model, &conn, &entry, err
	})
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// config builds a session's harness: the host machine in the session's
// working directory, and the model, instructions, tools, policy,
// subagents and limits of the session's agent. --model replaces the
// agent's spec.model and --mode its mode, each for this invocation or,
// when the session was created with it, for the whole session.
func (l *local) config(o runOptions) func(ctx context.Context, s session.Session) (harness.Config, error) {
	return func(ctx context.Context, s session.Session) (harness.Config, error) {
		ac, err := l.agentConfig(ctx, s)
		if err != nil {
			return harness.Config{}, err
		}
		var spec v1.AgentModel
		var overlay *models.Entry
		cfg := harness.Config{Prompt: prompts.HarnessOptions{Host: true}}
		if ac != nil {
			spec, overlay = ac.Model, &ac.Overlay
			cfg.Name, cfg.Instructions, cfg.Policy, cfg.Effort = ac.Name, ac.Instructions, ac.Policy, ac.Effort
			cfg.Subagents, cfg.MaxDepth, cfg.MaxConcurrent, cfg.CompactAt = ac.Subagents, ac.MaxDepth, ac.MaxConcurrent, ac.CompactAt
			cfg.Prompt.Threads = len(ac.Subagents) > 0
		}
		if name := cmp.Or(o.model, s.Metadata["model"]); name != "" {
			spec, overlay, cfg.Effort = v1.AgentModel{Name: name}, nil, ""
		}
		if spec.Name == "" {
			return harness.Config{}, &errUsage{"no model: pass --model or --agent"}
		}
		if cfg.Model, cfg.Connection, cfg.Entry, err = l.connect(ctx, spec, overlay); err != nil {
			return harness.Config{}, err
		}
		cfg.Policy.Mode = harness.Mode(cmp.Or(o.mode, s.Metadata["mode"], string(cfg.Policy.Mode), string(harness.ModeConfirm)))
		var roots []string
		held := manifest.Builtins()
		if ac != nil {
			roots, held = ac.Machine.Roots, ac.Tools
		}
		m, err := host.Open(host.Options{
			Workdir: s.Machine.Workdir, Roots: roots, SpillDir: filepath.Join(l.dataDir, "spill", s.ID),
			Home: config.Home(l.getenv), DataDir: l.dataDir, ID: s.ID, Owner: s.ID,
		})
		if err != nil {
			return harness.Config{}, err
		}
		if err := harness.CheckSandbox(cfg.Policy.Mode, m.Info()); err != nil {
			return harness.Config{}, errors.Join(&runner.SetupError{Code: harness.CodeSandboxUnavailable, Err: err}, m.Stop(ctx))
		}
		reg := tools.NewRegistry()
		for _, t := range tools.Builtins() {
			if !slices.Contains(held, t.Definition().Name) {
				continue
			}
			if err := reg.AddBuiltin(t); err != nil {
				return harness.Config{}, errors.Join(err, m.Stop(ctx))
			}
		}
		cfg.Machine, cfg.Tools = m, reg
		return cfg, nil
	}
}

func (l *local) drive(ctx context.Context, id string, o runOptions, env *cli) int {
	start, err := l.store.Get(ctx, id)
	if err != nil {
		return report(env, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := &printer{env: env, output: o.output}
	watch, err := l.store.Watch(ctx, id, start.LastSeq+1)
	if err != nil {
		return report(env, err)
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for e := range watch {
			out.event(e)
		}
	})
	sig := env.Signals
	if sig == nil {
		ch := make(chan os.Signal, 2)
		signal.Notify(ch, os.Interrupt)
		defer signal.Stop(ch)
		sig = ch
	}
	interrupted := make(chan struct{})
	go func() {
		n := 0
		for {
			select {
			case <-sig:
				n++
				if n == 1 {
					e, err := session.NewEvent(session.TypeUserInterrupt, session.UserInterrupt{Sender: l.person}, time.Now())
					if err == nil {
						err = l.append(context.WithoutCancel(ctx), id, e)
					}
					if err != nil {
						env.stderr.println("topos: interrupt:", err)
					}
					continue
				}
				close(interrupted)
				cancel()
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	r, err := l.runner(o)
	if err != nil {
		return report(env, err)
	}
	result, derr := r.Drive(ctx, id)
	cancel()
	wg.Wait()
	if rest, err := l.store.Events(context.WithoutCancel(ctx), id, out.next(start.LastSeq+1), 0); err == nil {
		for _, e := range rest {
			out.event(e)
		}
	} else if derr == nil {
		derr = err
	}
	select {
	case <-interrupted:
		return ExitInterrupted
	default:
	}
	if derr != nil {
		return report(env, derr)
	}
	out.finish(id, result)
	return exitCode(result)
}

// exitCode maps a turn's outcome to spec 024's exit codes.
func exitCode(o harness.Outcome) int {
	switch o.StopReason {
	case session.StopEndTurn, session.StopCompleted:
		return ExitOK
	case session.StopToolConfirmation, session.StopToolResult:
		return ExitWaiting
	case session.StopBudget, session.StopTurnLimit, session.StopOutputLimit:
		return ExitLimit
	case session.StopInterrupted:
		return ExitInterrupted
	}
	return ExitError
}

func report(env *cli, err error) int {
	env.stderr.println("topos:", err)
	var u *errUsage
	if errors.As(err, &u) {
		return ExitUsage
	}
	return ExitError
}

// printer writes a run's output as it arrives.
type printer struct {
	env    *cli
	output string
	mu     sync.Mutex
	last   uint64
	final  string
	calls  []session.AgentToolUse
}

// next is the first sequence the printer has not printed.
func (p *printer) next(from uint64) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return max(from, p.last+1)
}

func (p *printer) event(e session.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e.Seq <= p.last {
		return
	}
	p.last = e.Seq
	switch p.output {
	case "stream-json":
		b, err := session.Marshal(e)
		if err == nil {
			p.env.stdout.println(string(b))
		}
	}
	switch e.Type {
	case session.TypeAgentMessage:
		var m session.AgentMessage
		if e.Decode(&m) != nil {
			return
		}
		var text []string
		for _, b := range m.Message.Blocks {
			if b.Type == ir.BlockText && b.Text != "" {
				text = append(text, b.Text)
			}
		}
		if len(text) > 0 {
			p.final = strings.Join(text, "\n")
		}
	case session.TypeAgentToolUse:
		var u session.AgentToolUse
		if e.Decode(&u) != nil {
			return
		}
		p.calls = append(p.calls, u)
		if p.output == "text" {
			p.env.stderr.printf("%s %s [%s]\n", u.Name, summarize(u.Input), u.Verdict)
		}
	}
}

// summarize is one line of a call's input for the progress output.
func summarize(input json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	for _, k := range []string{"command", "path", "pattern", "url"} {
		if v, ok := in[k].(string); ok {
			if len(v) > 80 {
				v = v[:80] + "..."
			}
			return v
		}
	}
	return ""
}

func (p *printer) finish(id string, o harness.Outcome) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var pending []string
	if o.StopReason == session.StopToolConfirmation {
		for _, u := range p.calls {
			if u.Verdict == string(harness.VerdictAsk) {
				pending = append(pending, u.ToolUseID)
			}
		}
	}
	switch p.output {
	case "json":
		b, err := json.Marshal(map[string]any{"session_id": id, "status": o.Status, "stop_reason": o.StopReason, "detail": o.Detail, "text": p.final, "pending": pending})
		if err == nil {
			p.env.stdout.println(string(b))
		}
	case "text":
		if p.final != "" {
			p.env.stdout.println(p.final)
		}
		for _, id2 := range pending {
			p.env.stderr.printf("waiting for a confirmation: topos confirm %s %s allow|deny\n", id, id2)
		}
		if o.StopReason != session.StopEndTurn && o.StopReason != session.StopCompleted {
			p.env.stderr.printf("session %s stopped: %s %s\n", id, o.StopReason, o.Detail)
		}
	}
}
