// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package tasks is the task suite of spec 025: a fixed set of coding and
// file tasks, each a directory with its prompt, its starting files and
// its checker. A run copies a task's files into a fresh working
// directory, drives one session over it through the runner and the
// harness in process, and evaluates the checker on the final directory
// and the session's log. The aggregation of runs into pass rates, the
// release bar and the report are pure functions of the results.
package tasks

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// Categories are the directories of the suite, in the order of spec
// 025's category table.
var Categories = []string{"coding", "files", "instructions", "threads", "titles", "context"}

// The defaults of a task.yaml field that is left out.
const (
	DefaultTimeout = 30 * time.Minute
	DefaultMaxCost = 2.00
	DefaultRuns    = 3
)

// The files of a task directory.
const (
	FileTask      = "task.yaml"
	FileCheckYAML = "check.yaml"
	FileCheckSh   = "check.sh"
	DirFixture    = "fixture"
	FileBundle    = "fixture.bundle"
	FileSolution  = "solution.yaml"
	FileWrong     = "wrong.yaml"
)

// The placeholders a prompt, a check.yaml and a scripted script may
// hold, replaced for each run: the absolute working directory, with
// symbolic links evaluated as the host machine sees it, and the base
// URL of the pages a task serves.
const (
	PlaceholderWorkdir  = "${WORKDIR}"
	PlaceholderServeURL = "${SERVE_URL}"
)

// PlaceholderModel is what agent.title.model may be to name the run's own
// model as the agent's title model (spec 083), so a title is asked on the
// model the suite runs as an agent asks on a model it names.
const PlaceholderModel = "${MODEL}"

// Task is one task of the suite.
type Task struct {
	// ID is "<category>/<name>", the task's directory under the suite.
	ID       string
	Name     string
	Category string
	Prompt   string
	Agent    Agent
	Timeout  time.Duration
	// MaxCostUSDMicro caps one run's spend.
	MaxCostUSDMicro int64
	Runs            int
	// Serve is a directory of the task served over loopback for each
	// run, at the URL the placeholder ${SERVE_URL} names; empty serves
	// nothing.
	Serve string
	// Search is a file of the task holding the results the suite's
	// search service answers every web_search with, ${SERVE_URL}
	// expanded (spec 047); a task whose agent holds web_search names one.
	Search string
	// Log is a file of the task holding JSON lines of session events,
	// placeholders expanded, which the run writes into the session
	// before it sends the prompt, so the run starts in the middle of a
	// conversation; empty starts the session with the prompt.
	Log string
	// Dir is the task's directory.
	Dir string
	// Bundle is set when the starting files are a git bundle rather
	// than a fixture directory.
	Bundle bool
	// Check is the check.yaml source, placeholders unexpanded; empty
	// when the task's checker is check.sh.
	Check []byte
	// Answers are what the run's driver answers a question with, as a
	// person would. A task that names any runs in an attended session,
	// and every other task in one nobody attends, where a question is
	// answered at once.
	Answers []Answer
	// Confirm is what the run's driver answers a call held for a person
	// with, as that person would: session.DecisionAllow or DecisionDeny,
	// once for each call the session waits on; empty answers none, and a
	// run that waits on a call fails.
	Confirm string
	// Woken is WokenJudged for a task whose prompt arrives as a judged
	// trigger's message, which nobody waits for (spec 086); empty sends it
	// as the person's.
	Woken string
}

// WokenJudged is Task.Woken for a prompt a judged trigger sends.
const WokenJudged = "judged"

// Answer is what the driver answers to the questions of the open
// question call whose header or text contains Question, compared without
// case: the option whose label contains Choose, or Choose itself in the
// person's own words when no label holds it, and Text as the person's
// words. A question no Answer names is left to the agent.
type Answer struct {
	Question string `yaml:"question"`
	Choose   string `yaml:"choose"`
	Text     string `yaml:"text"`
}

// Agent overrides the suite's agent for one task: the built-in tools it
// holds, its own instructions, the subagents its threads may spawn, and
// its title setting.
type Agent struct {
	// Tools are the tools the agent holds, built-ins and the question
	// tool; nil holds every built-in, and not the question tool.
	Tools        []string            `yaml:"tools"`
	Instructions string              `yaml:"instructions"`
	Subagents    map[string]Subagent `yaml:"subagents"`
	// Title is the agent's spec.title (spec 083): set, the session stays
	// idle at the end of its turn instead of ending, and the runner asks
	// for its title after the turn, the question allowed and naming no
	// model. A title model of PlaceholderModel is the run's own model,
	// named as the agent's title model.
	Title *Title `yaml:"title"`
}

// Title is spec.title as a task names it.
type Title struct {
	Model *string `yaml:"model"`
}

// setting is the title setting the run's runner reads, nil for none, a
// title model of PlaceholderModel being model, the run's own.
func (a Agent) setting(model string) *v1.Title {
	if a.Title == nil {
		return nil
	}
	set := &v1.Title{Model: a.Title.Model}
	if m := a.Title.Model; m != nil && *m == PlaceholderModel {
		set.Model = &model
	}
	return set
}

// Subagent is an agent a task's threads may spawn.
type Subagent struct {
	Instructions string `yaml:"instructions"`
	// Tools are the built-ins the subagent holds; nil holds the parent's.
	Tools []string `yaml:"tools"`
}

// taskFile is task.yaml as written.
type taskFile struct {
	Name     string   `yaml:"name"`
	Category string   `yaml:"category"`
	Prompt   string   `yaml:"prompt"`
	Agent    Agent    `yaml:"agent"`
	Timeout  string   `yaml:"timeout"`
	MaxCost  *float64 `yaml:"maxCost"`
	Runs     *int     `yaml:"runs"`
	Serve    string   `yaml:"serve"`
	Search   string   `yaml:"search"`
	Log      string   `yaml:"log"`
	Answers  []Answer `yaml:"answers"`
	Confirm  string   `yaml:"confirm"`
	Woken    string   `yaml:"woken"`
}

// Load reads every task of the suite rooted at dir: each directory
// <category>/<name> holding a task.yaml, in the order of Categories and
// then by name.
func Load(dir string) ([]Task, error) {
	var out []Task
	for _, c := range Categories {
		entries, err := os.ReadDir(filepath.Join(dir, c))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("tasks: %w", err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			t, err := LoadTask(filepath.Join(dir, c, e.Name()))
			if err != nil {
				return nil, err
			}
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("tasks: no task under %s", dir)
	}
	return out, nil
}

// LoadTask reads one task directory and checks it: its name and
// category agree with its path, it has a prompt, its tools are
// built-ins, and it has exactly one checker whose check.yaml parses.
func LoadTask(dir string) (Task, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return Task{}, fmt.Errorf("tasks: %w", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, FileTask))
	if err != nil {
		return Task{}, fmt.Errorf("tasks: %w", err)
	}
	var f taskFile
	if err := yaml.UnmarshalWithOptions(b, &f, yaml.DisallowUnknownField()); err != nil {
		return Task{}, fmt.Errorf("tasks: %s: %w", filepath.Join(dir, FileTask), err)
	}
	t := Task{
		Name: f.Name, Category: f.Category, Prompt: strings.TrimSpace(f.Prompt), Agent: f.Agent,
		Timeout: DefaultTimeout, MaxCostUSDMicro: int64(math.Round(DefaultMaxCost * 1e6)), Runs: DefaultRuns,
		Serve: f.Serve, Search: f.Search, Log: f.Log, Dir: dir, Answers: f.Answers, Confirm: f.Confirm, Woken: f.Woken,
	}
	t.ID = t.Category + "/" + t.Name
	fail := func(format string, a ...any) (Task, error) {
		return Task{}, fmt.Errorf("tasks: %s: %s", filepath.Join(dir, FileTask), fmt.Sprintf(format, a...))
	}
	switch {
	case t.Name != filepath.Base(dir):
		return fail("name %q is not the directory's name %q", t.Name, filepath.Base(dir))
	case t.Category != filepath.Base(filepath.Dir(dir)) || !slices.Contains(Categories, t.Category):
		return fail("category %q is not the parent directory %q among %v", t.Category, filepath.Base(filepath.Dir(dir)), Categories)
	case t.Prompt == "":
		return fail("no prompt")
	}
	if f.Timeout != "" {
		if t.Timeout, err = time.ParseDuration(f.Timeout); err != nil || t.Timeout <= 0 {
			return fail("timeout %q is not a positive duration", f.Timeout)
		}
	}
	if f.MaxCost != nil {
		if *f.MaxCost <= 0 || math.IsInf(*f.MaxCost, 0) || math.IsNaN(*f.MaxCost) {
			return fail("maxCost %v is not a positive amount in USD", *f.MaxCost)
		}
		t.MaxCostUSDMicro = int64(math.Round(*f.MaxCost * 1e6))
	}
	if f.Runs != nil {
		if *f.Runs < 1 {
			return fail("runs %d is not at least 1", *f.Runs)
		}
		t.Runs = *f.Runs
	}
	if err := checkTools(t.Agent); err != nil {
		return fail("%v", err)
	}
	if m := t.Agent.Title; m != nil && m.Model != nil && strings.TrimSpace(*m.Model) == "" {
		return fail("agent.title.model is empty: name a model, or leave model out for the run's own")
	}
	if len(t.Answers) > 0 && !slices.Contains(t.Agent.Tools, harness.ToolQuestion) {
		return fail("answers are for a question, and the agent does not hold the question tool")
	}
	for i, a := range t.Answers {
		if a.Question == "" || (a.Choose == "" && a.Text == "") {
			return fail("answers[%d] names a question and what to choose or say", i)
		}
	}
	if t.Confirm != "" && t.Confirm != session.DecisionAllow && t.Confirm != session.DecisionDeny {
		return fail("confirm %q is %s or %s", t.Confirm, session.DecisionAllow, session.DecisionDeny)
	}
	if t.Woken != "" && t.Woken != WokenJudged {
		return fail("woken is %q, not %s", t.Woken, WokenJudged)
	}
	if t.Serve != "" {
		if fi, err := os.Stat(filepath.Join(dir, t.Serve)); err != nil || !fi.IsDir() {
			return fail("serve %q is not a directory of the task", t.Serve)
		}
	}
	searches := slices.Contains(t.Agent.Tools, tools.NameWebSearch)
	switch {
	case t.Search != "" && !searches:
		return fail("search is the results of web_search, and the agent does not hold it")
	case searches && t.Search == "":
		return fail("the agent holds web_search, and the task names no search results")
	case t.Search != "":
		if _, err := readHits(filepath.Join(dir, t.Search), ""); err != nil {
			return fail("search %q: %v", t.Search, err)
		}
	}
	if t.Log != "" {
		if _, err := readLog(filepath.Join(dir, t.Log), "/work", "http://127.0.0.1:1", time.Now()); err != nil {
			return fail("log %q: %v", t.Log, err)
		}
	}
	if err := t.findStart(); err != nil {
		return fail("%v", err)
	}
	if err := t.findChecker(); err != nil {
		return fail("%v", err)
	}
	return t, nil
}

// builtinNames are the names of the built-in tools.
func builtinNames() []string {
	var names []string
	for _, b := range tools.Builtins() {
		names = append(names, b.Definition().Name)
	}
	return names
}

// checkTools refuses a tool list naming anything but a built-in, one
// an agent opts into, or the question, trigger, wake and notify tools.
func checkTools(a Agent) error {
	known := slices.Concat(builtinNames(), tools.OptIn(), []string{harness.ToolQuestion, session.ToolTrigger, session.ToolWake, session.ToolNotify})
	lists := map[string][]string{"agent": a.Tools}
	for name, s := range a.Subagents {
		lists["subagent "+name] = s.Tools
	}
	for who, list := range lists {
		for _, n := range list {
			if !slices.Contains(known, n) {
				return fmt.Errorf("the %s holds %q, which is not a built-in tool, web_search, history, question, trigger, wake or notify (%s)", who, n, strings.Join(known, ", "))
			}
		}
	}
	return nil
}

// findStart finds the starting files: a fixture directory, a git
// bundle, or neither for a task that starts empty, never both.
func (t *Task) findStart() error {
	_, derr := os.Stat(filepath.Join(t.Dir, DirFixture))
	_, berr := os.Stat(filepath.Join(t.Dir, FileBundle))
	if derr == nil && berr == nil {
		return fmt.Errorf("both %s/ and %s are present; a task starts from one", DirFixture, FileBundle)
	}
	t.Bundle = berr == nil
	return nil
}

// findChecker finds the one checker, check.yaml or check.sh, and parses
// check.yaml with its placeholders standing for sample values.
func (t *Task) findChecker() error {
	b, err := os.ReadFile(filepath.Join(t.Dir, FileCheckYAML))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	hasYAML := err == nil
	_, err = os.Stat(filepath.Join(t.Dir, FileCheckSh))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	hasSh := err == nil
	switch {
	case hasYAML && hasSh:
		return fmt.Errorf("both %s and %s are present; a task has one checker", FileCheckYAML, FileCheckSh)
	case !hasYAML && !hasSh:
		return fmt.Errorf("no checker: write %s or %s", FileCheckYAML, FileCheckSh)
	case hasSh:
		return nil
	}
	if _, err := ParseCheck(expand(b, "/work", "http://127.0.0.1:1")); err != nil {
		return err
	}
	t.Check = b
	return nil
}

// expand replaces the placeholders.
func expand(b []byte, workdir, serveURL string) []byte {
	return []byte(strings.NewReplacer(PlaceholderWorkdir, workdir, PlaceholderServeURL, serveURL).Replace(string(b)))
}
