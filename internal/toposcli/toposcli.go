// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package toposcli is the topos command (spec 024): it runs a local
// session in print mode in the working directory, over the directory
// store, and continues it after a confirmation. It has no interactive
// terminal.
package toposcli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/prompt"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/internal/version"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/host"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/dialect"
	"latere.ai/x/topos/models/scripted"
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
  topos confirm <session> <tool_use_id> allow|deny [--note <text>] [--remember <pattern>]
  topos version
`)
}

// runOptions are the flags of run and confirm.
type runOptions struct {
	session string
	model   string
	mode    string
	maxCost float64
	dir     string
	output  string
}

func (o *runOptions) flags(fs *flag.FlagSet) {
	fs.StringVar(&o.session, "session", "", "continue this session")
	fs.StringVar(&o.model, "model", "", "the model to run, by catalog name")
	fs.StringVar(&o.mode, "mode", string(harness.ModeConfirm), "plan, confirm or progressive")
	fs.Float64Var(&o.maxCost, "max-cost", 0, "the session's budget in USD; 0 is none")
	fs.StringVar(&o.dir, "dir", "", "the working directory; empty is the current one")
	fs.StringVar(&o.output, "output", "text", "text, json or stream-json")
}

func (o runOptions) validate() error {
	switch harness.Mode(o.mode) {
	case harness.ModePlan, harness.ModeConfirm, harness.ModeProgressive:
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
	l, err := openLocal(env, o)
	if err != nil {
		return report(env, err)
	}
	id := o.session
	if id == "" {
		s, err := l.create(ctx, o)
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
}

// DataDir is TOPOS_DATA_DIR, or $XDG_STATE_HOME/topos, or
// $HOME/.local/state/topos.
func DataDir(getenv func(string) string) (string, error) {
	if d := getenv("TOPOS_DATA_DIR"); d != "" {
		return d, nil
	}
	if d := getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "topos"), nil
	}
	if h := getenv("HOME"); h != "" {
		return filepath.Join(h, ".local", "state", "topos"), nil
	}
	return "", errors.New("no data directory: set TOPOS_DATA_DIR or HOME")
}

func openLocal(env *cli, o runOptions) (*local, error) {
	dataDir, err := DataDir(env.Getenv)
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

func (l *local) create(ctx context.Context, o runOptions) (session.Session, error) {
	s := session.New(session.AgentRef{ID: builtinAgent, Name: "topos", Version: 1}, l.person, session.RunnerExternal,
		session.Machine{Kind: machine.KindHost, Workdir: l.workdir}, time.Now())
	s.Writer = &session.Writer{Kind: session.RunnerExternal, Subject: l.person.Subject, Since: s.CreatedAt}
	if o.maxCost > 0 {
		micro := int64(math.Ceil(o.maxCost * 1e6))
		s.Budget.MaxCostUSDMicro = &micro
	}
	s.Metadata = map[string]string{"model": o.model, "mode": o.mode}
	if err := l.store.Create(ctx, s, nil); err != nil {
		return session.Session{}, err
	}
	return s, nil
}

// builtinAgent is the agent a run with no manifest runs.
const builtinAgent = "agent_00000000000000000000000000"

// append adds one event after the session's last sequence.
func (l *local) append(ctx context.Context, id string, e session.Event) error {
	s, err := l.store.Get(ctx, id)
	if err != nil {
		return err
	}
	evs := []session.Event{e}
	session.Stamp(id, s.LastSeq, evs)
	_, err = l.store.Append(ctx, id, s.LastSeq, evs)
	return err
}

// scriptedEntry is the figures of a scripted model, which no catalog
// names: windows large enough for any test, and no price.
func scriptedEntry(name string) models.Entry {
	return models.Entry{Name: name, Family: models.FamilyOther, InputWindow: 200_000, MaxOutputTokens: 32_000}
}

// errUsage is a usage error found after the flags parsed.
type errUsage struct{ msg string }

func (e *errUsage) Error() string { return e.msg }

// config builds a session's harness: the host machine in the session's
// working directory, the built-in tools, and the model the run names.
func (l *local) config(o runOptions) func(ctx context.Context, s session.Session) (harness.Config, error) {
	return func(ctx context.Context, s session.Session) (harness.Config, error) {
		name := o.model
		if name == "" {
			name = s.Metadata["model"]
		}
		if name == "" {
			return harness.Config{}, &errUsage{"no model: pass --model or --agent"}
		}
		base := l.getenv("TOPOS_MODELS_URL")
		if base == "" {
			return harness.Config{}, &errUsage{"no model connection: set TOPOS_MODELS_URL"}
		}
		conn := models.Connection{BaseURL: base, Model: name, Credential: l.getenv("TOPOS_MODELS_KEY")}
		model := l.model
		var entry models.Entry
		if conn.Scripted() {
			entry, model = scriptedEntry(name), l.scripted
		} else {
			cat, err := models.Embedded()
			if err != nil {
				return harness.Config{}, err
			}
			if entry, err = cat.Resolve(name); err != nil {
				return harness.Config{}, err
			}
		}
		conn.Family, conn.Dialect = entry.Family, entry.Dialect
		mode := o.mode
		if mode == "" {
			mode = s.Metadata["mode"]
		}
		m, err := host.Open(host.Options{
			Workdir: s.Machine.Workdir, SpillDir: filepath.Join(l.dataDir, "spill", s.ID),
			Home: l.getenv("HOME"), DataDir: l.dataDir, ID: s.ID,
		})
		if err != nil {
			return harness.Config{}, err
		}
		reg := tools.NewRegistry()
		for _, t := range tools.Builtins() {
			if err := reg.AddBuiltin(t); err != nil {
				return harness.Config{}, errors.Join(err, m.Release(ctx, true))
			}
		}
		return harness.Config{
			Model:      model,
			Connection: conn,
			Entry:      entry,
			Machine:    m,
			Tools:      reg,
			Policy:     harness.Policy{Mode: harness.Mode(mode)},
			Prompt:     prompt.Options{Host: true},
		}, nil
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
	r, err := runner.New(runner.Options{Store: l.store, Harness: l.config(o), ID: "topos-" + fmt.Sprint(os.Getpid()), Kind: runner.KindLocal})
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
