// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/prompt"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/host"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/dialect"
	"latere.ai/x/topos/models/scripted"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/dir"
)

// Options configure a run of the suite.
type Options struct {
	// Dir is the suite's root, holding the category directories; empty
	// is the directory this package was built from, which exists in a
	// checkout.
	Dir string
	// Work holds each run's working directory, its session data and the
	// checker's scratch space, and is kept as the run's artifact. It
	// must not be inside a git checkout, whose instruction files and
	// checkpoints would join every session.
	Work string
	// Model streams the requests; nil is the HTTP model of
	// models/dialect.
	Model models.Model
	// Connection names the model. Its family and dialect come from the
	// catalog when empty.
	Connection models.Connection
	// Entry overlays the catalog's figures for the connection's model,
	// for a model the catalog does not know.
	Entry *models.Entry
	// Script, when set, plays the file of that name in each task's
	// directory through the scripted model in place of Model and
	// Connection: solution.yaml or wrong.yaml.
	Script string
	// Machine opens a run's machine; nil opens the host machine.
	Machine func(ctx context.Context, m MachineOptions) (machine.Machine, error)
	// Filter selects the tasks whose ID matches this regular
	// expression; empty selects every task.
	Filter string
	// Runs overrides each task's number of runs; zero keeps it.
	Runs int
	// BudgetUSDMicro caps the suite's total spend; zero is no cap.
	BudgetUSDMicro int64
	// Commit names the commit under test in the report.
	Commit string
	// OnRun is told each run's result as it finishes.
	OnRun func(RunResult)
}

// MachineOptions are what a run's machine is opened with.
type MachineOptions struct {
	Workdir  string
	SpillDir string
	DataDir  string
	Home     string
	ID       string
	// TempDir is the TMPDIR of the machine's commands: a directory of
	// the run, so that what a command leaves behind, such as the build
	// directory of a go run the session end stopped, stays in the run's
	// artifact and never reaches the system's temporary directory.
	TempDir string
}

// RunGrace is how long a run may pass its task's timeout before its
// context is canceled: the turn deadline stops the turn at the next
// step boundary, and this bounds a step that never reaches one.
const RunGrace = 2 * time.Minute

// RunnerID names the suite's runner in the sessions it drives.
const RunnerID = "topos-tasks"

// budgetCodes are the error types Lux answers when a key's spend is
// refused: a hard budget exhausted, or the key's own spend window.
var budgetCodes = []string{"budget_exhausted", "spend_exceeded"}

// ScriptedEntry is the catalog figures of the scripted model: windows
// large enough for any task, and zero prices, so a budget applies and
// the cost is the script's own cost_usd_micro.
func ScriptedEntry(name string) models.Entry {
	zero := models.Price(0)
	return models.Entry{
		Name: name, Family: models.FamilyOther, InputWindow: 200_000, MaxOutputTokens: 64_000,
		Pricing: &models.Pricing{Input: &zero, Output: &zero},
	}
}

// Run runs the selected tasks, each its number of runs, one run at a
// time, and returns the report. A run that fails for any reason is a
// failed run; Run returns an error only when the suite cannot start or
// the context ends. When the suite's budget is spent, or the model's
// key is refused for spend, the remaining runs are not started and the
// report is incomplete.
func Run(ctx context.Context, o Options) (Report, error) {
	selected, err := o.tasks()
	if err != nil {
		return Report{}, err
	}
	if err := o.checkWork(ctx); err != nil {
		return Report{}, err
	}
	start := time.Now()
	rep := Report{Model: o.modelName(), Commit: o.Commit, Started: start.UTC()}
	for _, t := range selected {
		n := t.Runs
		if o.Runs > 0 {
			n = o.Runs
		}
		for i := 1; i <= n; i++ {
			if o.BudgetUSDMicro > 0 && rep.SpendUSDMicro >= o.BudgetUSDMicro {
				rep.incomplete(fmt.Sprintf("the suite's budget of %s is spent", usd(o.BudgetUSDMicro)))
				return rep.finish(start), nil
			}
			r, err := RunTask(ctx, t, o, i)
			if ctx.Err() != nil {
				return rep.finish(start), ctx.Err()
			}
			if err != nil {
				r = RunResult{Task: t.ID, Run: i, Reason: "the suite could not run it: " + err.Error()}
			}
			rep.Runs = append(rep.Runs, r)
			rep.SpendUSDMicro += r.CostUSDMicro
			if o.OnRun != nil {
				o.OnRun(r)
			}
			if r.BudgetExhausted {
				rep.incomplete("the model's key was refused for spend: " + r.Error)
				return rep.finish(start), nil
			}
		}
	}
	return rep.finish(start), nil
}

// tasks loads the suite and applies the filter.
func (o Options) tasks() ([]Task, error) {
	d := o.Dir
	if d == "" {
		d = sourceDir()
	}
	all, err := Load(d)
	if err != nil {
		return nil, err
	}
	if o.Filter == "" {
		return all, nil
	}
	re, err := regexp.Compile(o.Filter)
	if err != nil {
		return nil, fmt.Errorf("tasks: the filter: %w", err)
	}
	var out []Task
	for _, t := range all {
		if re.MatchString(t.ID) {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("tasks: no task matches %q", o.Filter)
	}
	return out, nil
}

// sourceDir is the directory this file was compiled from, or the
// working directory when that is not a directory on this disk, as in a
// build with -trimpath, where go test runs in the package's directory.
func sourceDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	if fi, err := os.Stat(filepath.Dir(file)); err != nil || !fi.IsDir() {
		return "."
	}
	return filepath.Dir(file)
}

// checkWork refuses a work directory that does not exist or sits
// inside a git work tree.
func (o Options) checkWork(ctx context.Context) error {
	if o.Work == "" {
		return errors.New("tasks: no work directory")
	}
	if fi, err := os.Stat(o.Work); err != nil || !fi.IsDir() {
		return fmt.Errorf("tasks: the work directory %s is not a directory", o.Work)
	}
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = o.Work
	if out, err := cmd.Output(); err == nil && strings.TrimSpace(string(out)) == "true" {
		return fmt.Errorf("tasks: the work directory %s is inside a git checkout; its instruction files and checkpoints would join every session", o.Work)
	}
	return nil
}

func (o Options) modelName() string {
	if o.Script != "" {
		return models.SchemeScripted + ":" + o.Script
	}
	return o.Connection.Model
}

// RunResult is one run of one task.
type RunResult struct {
	Task string `json:"task"`
	Run  int    `json:"run"`
	// Passed is true when the session ended its turn, stayed within
	// its task's maxCost, and the checker passed.
	Passed bool `json:"passed"`
	// Reason is why a run failed.
	Reason  string             `json:"reason,omitempty"`
	Verdict *Verdict           `json:"verdict,omitempty"`
	Stop    session.StopReason `json:"stop_reason,omitempty"`
	Detail  string             `json:"detail,omitempty"`
	// Error is the message of the session's last session.error.
	Error string `json:"error,omitempty"`
	// Steps are the model requests of every thread.
	Steps           int   `json:"steps"`
	ToolCalls       int   `json:"tool_calls"`
	CostUSDMicro    int64 `json:"cost_usd_micro"`
	MaxCostUSDMicro int64 `json:"max_cost_usd_micro"`
	// BudgetExhausted is a model key refused for spend, which ends the
	// suite incomplete.
	BudgetExhausted bool   `json:"budget_exhausted,omitempty"`
	DurationMS      int64  `json:"duration_ms"`
	ServeURL        string `json:"serve_url,omitempty"`
	Session         string `json:"session,omitempty"`
	Log             string `json:"log,omitempty"`
	Workdir         string `json:"workdir,omitempty"`
}

// run is one run in progress.
type run struct {
	o        Options
	t        Task
	base     string
	workdir  string
	data     string
	serveURL string

	mu sync.Mutex
	m  machine.Machine
}

// RunTask runs a task once, as its n-th run: it copies the starting
// files into a fresh working directory, drives one session over it to
// the end of its turn, and evaluates the checker. How the session ended
// and what the checker said are in the result; the error is a run the
// suite could not set up, drive or judge.
func RunTask(ctx context.Context, t Task, o Options, n int) (RunResult, error) {
	started := time.Now()
	base, err := os.MkdirTemp(o.Work, fmt.Sprintf("%s-%s-%d-", t.Category, t.Name, n))
	if err != nil {
		return RunResult{}, fmt.Errorf("tasks: %w", err)
	}
	if base, err = filepath.EvalSymlinks(base); err != nil {
		return RunResult{}, fmt.Errorf("tasks: %w", err)
	}
	r := &run{o: o, t: t, base: base, workdir: filepath.Join(base, "work"), data: filepath.Join(base, "data")}
	if err := r.start(ctx); err != nil {
		return RunResult{}, err
	}
	if t.Serve != "" {
		srv := httptest.NewServer(http.FileServer(http.Dir(filepath.Join(t.Dir, t.Serve))))
		defer srv.Close()
		r.serveURL = srv.URL
	}
	res, err := r.drive(ctx)
	res.Task, res.Run, res.MaxCostUSDMicro = t.ID, n, t.MaxCostUSDMicro
	res.DurationMS = time.Since(started).Milliseconds()
	return res, err
}

// start lays out the starting files: the fixture copied, the bundle
// cloned, or an empty directory.
func (r *run) start(ctx context.Context) error {
	if r.t.Bundle {
		cmd := exec.CommandContext(ctx, "git", "clone", "--quiet", filepath.Join(r.t.Dir, FileBundle), r.workdir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("tasks: clone %s: %w: %s", FileBundle, err, out)
		}
		return nil
	}
	fixture := filepath.Join(r.t.Dir, DirFixture)
	_, err := os.Stat(fixture)
	switch {
	case err == nil:
		return copyTree(fixture, r.workdir)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("tasks: %w", err)
	}
	return os.MkdirAll(r.workdir, 0o755)
}

// connection is the run's model, connection and catalog figures.
func (r *run) connection() (models.Model, models.Connection, models.Entry, error) {
	if r.o.Script != "" {
		path := filepath.Join(r.t.Dir, r.o.Script)
		m := &scripted.Model{Load: func(p string) ([]byte, error) {
			b, err := os.ReadFile(p)
			return expand(b, r.workdir, r.serveURL), err
		}}
		return m, models.Connection{BaseURL: models.SchemeScripted + ":" + path, Model: "scripted"}, ScriptedEntry("scripted"), nil
	}
	conn := r.o.Connection
	cat, err := models.Embedded()
	if err != nil {
		return nil, conn, models.Entry{}, err
	}
	var over []models.Entry
	if r.o.Entry != nil {
		over = append(over, *r.o.Entry)
	}
	entry, err := cat.Resolve(conn.Model, over...)
	if err != nil {
		return nil, conn, models.Entry{}, err
	}
	if conn.Family == "" {
		conn.Family = entry.Family
	}
	if conn.Dialect == "" {
		conn.Dialect = entry.Dialect
	}
	m := r.o.Model
	if m == nil {
		m = &dialect.Model{}
	}
	return m, conn, entry, nil
}

// config builds the session's harness: the machine in the working
// directory, the task's tools, every call allowed, and the task's
// instructions and subagents.
func (r *run) config(model models.Model, conn models.Connection, entry models.Entry) func(ctx context.Context, s session.Session) (harness.Config, error) {
	return func(ctx context.Context, s session.Session) (harness.Config, error) {
		open := r.o.Machine
		if open == nil {
			open = openHost
		}
		m, err := open(ctx, MachineOptions{
			Workdir: s.Machine.Workdir, SpillDir: filepath.Join(r.data, "spill", s.ID),
			DataDir: r.data, Home: os.Getenv("HOME"), ID: s.ID, TempDir: filepath.Join(r.base, "tmp"),
		})
		if err != nil {
			return harness.Config{}, err
		}
		r.mu.Lock()
		r.m = m
		r.mu.Unlock()
		reg := tools.NewRegistry()
		for _, b := range tools.Builtins() {
			if r.t.Agent.Tools != nil && !slices.Contains(r.t.Agent.Tools, b.Definition().Name) {
				continue
			}
			if err := reg.AddBuiltin(b); err != nil {
				return harness.Config{}, err
			}
		}
		var subs map[string]harness.Subagent
		for name, sa := range r.t.Agent.Subagents {
			if subs == nil {
				subs = map[string]harness.Subagent{}
			}
			subs[name] = harness.Subagent{Name: name, Instructions: sa.Instructions, Tools: sa.Tools}
		}
		return harness.Config{
			Model: model, Connection: conn, Entry: entry, Machine: m, Tools: reg,
			Policy:       harness.Policy{Mode: harness.ModeConfirm, AlwaysAllow: builtinNames()},
			Name:         "topos",
			Instructions: r.t.Agent.Instructions,
			Subagents:    subs,
			TurnTimeout:  r.t.Timeout,
			Prompt:       prompt.Options{Host: m.Info().Kind == machine.KindHost, Threads: len(subs) > 0},
		}, nil
	}
}

// openHost opens the host machine with this process's environment, the
// person's own, but for TMPDIR.
func openHost(_ context.Context, o MachineOptions) (machine.Machine, error) {
	env, err := withTempDir(os.Environ(), o.TempDir)
	if err != nil {
		return nil, err
	}
	return host.Open(host.Options{Workdir: o.Workdir, SpillDir: o.SpillDir, Home: o.Home, DataDir: o.DataDir, ID: o.ID, Environ: env})
}

// withTempDir creates dir and makes it the environment's TMPDIR.
func withTempDir(environ []string, dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("tasks: the temporary directory: %w", err)
	}
	out := slices.DeleteFunc(slices.Clone(environ), func(kv string) bool { return strings.HasPrefix(kv, "TMPDIR=") })
	return append(out, "TMPDIR="+dir), nil
}

// release ends the machine, which stops the run's background jobs.
func (r *run) release(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		return nil
	}
	return r.m.Release(ctx, true)
}

// drive runs the session and judges it.
func (r *run) drive(ctx context.Context) (res RunResult, err error) {
	model, conn, entry, err := r.connection()
	if err != nil {
		return RunResult{}, err
	}
	st, err := dir.Open(r.data)
	if err != nil {
		return RunResult{}, err
	}
	now := time.Now()
	person := session.Sender{Subject: "local:tasks", Name: "tasks", Kind: session.SenderPerson}
	s := session.New(session.AgentRef{ID: session.NewID(session.PrefixAgent), Name: "topos", Version: 1}, person, session.RunnerExternal,
		session.Machine{Kind: machine.KindHost, Workdir: r.workdir}, now)
	s.Budget.MaxCostUSDMicro = &r.t.MaxCostUSDMicro
	s.EndOnIdle = true
	s.Limits.TurnTimeout = r.t.Timeout.String()
	s.Metadata = map[string]string{"task": r.t.ID, "model": conn.Model}
	if err := st.Create(ctx, s, nil); err != nil {
		return RunResult{}, err
	}
	res.Session, res.Workdir, res.MaxCostUSDMicro = s.ID, r.workdir, r.t.MaxCostUSDMicro
	res.Log = filepath.Join(r.data, "sessions", s.ID, "events.jsonl")
	text := string(expand([]byte(r.t.Prompt), r.workdir, r.serveURL))
	msg, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: person, Content: []lux.Block{{Type: ir.BlockText, Text: text}}}, now)
	if err != nil {
		return res, err
	}
	evs := []session.Event{msg}
	session.Stamp(s.ID, 0, evs)
	if _, err := st.Append(ctx, s.ID, 0, evs); err != nil {
		return res, err
	}
	rn, err := runner.New(runner.Options{
		Store: st, Harness: r.config(model, conn, entry), ID: RunnerID, Kind: runner.KindLocal,
		CheckpointDir: filepath.Join(r.data, "checkpoints"),
	})
	if err != nil {
		return res, err
	}
	dctx, cancel := context.WithTimeout(ctx, r.t.Timeout+RunGrace)
	out, derr := rn.Drive(dctx, s.ID)
	cancel()
	if err := r.release(context.WithoutCancel(ctx)); err != nil {
		derr = errors.Join(derr, err)
	}
	log, err := st.Events(context.WithoutCancel(ctx), s.ID, 1, 0)
	if err != nil {
		return res, err
	}
	r.measure(&res, log)
	res.Stop, res.Detail = out.StopReason, out.Detail
	if res.Reason = failure(derr, res); res.Reason != "" {
		return res, nil
	}
	res.ServeURL = r.serveURL
	v, err := judge(ctx, r.t, res, log, filepath.Join(r.base, "check"))
	if err != nil {
		return res, err
	}
	res.Verdict = &v
	res.Passed = v.Pass
	if !v.Pass {
		res.Reason = fmt.Sprintf("the checker failed at %s assertion %d: %s", v.Kind, v.Assertion, v.Reason)
	}
	return res, nil
}

// failure is why a run fails before its checker runs: the session
// failed, it stopped other than at the end of its turn, or it spent
// past its maxCost. A failed session is a failed run, not an error of
// the suite.
func failure(sessionErr error, res RunResult) string {
	switch {
	case sessionErr != nil:
		return "the session failed: " + sessionErr.Error()
	case res.Stop != session.StopEndTurn && res.Stop != session.StopCompleted:
		reason := strings.TrimSpace("the session stopped " + string(res.Stop) + " " + res.Detail)
		if res.Error != "" {
			reason += ": " + res.Error
		}
		return reason
	case res.CostUSDMicro > res.MaxCostUSDMicro:
		return fmt.Sprintf("the run spent %s, past its maxCost of %s", usd(res.CostUSDMicro), usd(res.MaxCostUSDMicro))
	}
	return ""
}

// Judge evaluates a task's checker again over a finished run: its final
// working directory and its session's log as the run left them, with
// scratch as the checker's own space, which must not exist yet.
func Judge(ctx context.Context, t Task, res RunResult, scratch string) (Verdict, error) {
	if res.Log == "" || res.Session == "" {
		return Verdict{}, errors.New("tasks: the run has no session log")
	}
	st, err := dir.Open(filepath.Dir(filepath.Dir(filepath.Dir(res.Log))))
	if err != nil {
		return Verdict{}, err
	}
	log, err := st.Events(ctx, res.Session, 1, 0)
	if err != nil {
		return Verdict{}, err
	}
	return judge(ctx, t, res, log, scratch)
}

func judge(ctx context.Context, t Task, res RunResult, log []session.Event, scratch string) (Verdict, error) {
	if err := os.Mkdir(scratch, 0o755); err != nil {
		return Verdict{}, fmt.Errorf("tasks: the checker's scratch directory: %w", err)
	}
	return Evaluate(ctx, Input{Workdir: res.Workdir, Task: t, Log: res.Log, Events: log, Scratch: scratch, ServeURL: res.ServeURL})
}

// measure reads the steps, the calls, the cost and a spend refusal from
// the log.
func (r *run) measure(res *RunResult, log []session.Event) {
	for _, e := range log {
		switch e.Type {
		case session.TypeModelRequest:
			res.Steps++
			var p session.ModelRequest
			if e.Decode(&p) == nil && p.CostUSDMicro != nil {
				res.CostUSDMicro += *p.CostUSDMicro
			}
		case session.TypeAgentToolUse:
			res.ToolCalls++
		case session.TypeSessionError:
			var p session.SessionError
			if e.Decode(&p) != nil {
				continue
			}
			res.Error = p.Message
			if p.Code != harness.CodeModelError {
				continue
			}
			for _, c := range budgetCodes {
				if strings.Contains(p.Detail, c) || strings.Contains(p.Message, c) {
					res.BudgetExhausted = true
				}
			}
		}
	}
}

// usd renders micro-USD as dollars.
func usd(micro int64) string {
	return fmt.Sprintf("$%.4f", float64(micro)/1e6)
}
