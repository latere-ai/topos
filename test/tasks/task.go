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

	"latere.ai/x/topos/harness/tools"
)

// Categories are the directories of the suite, in the order of spec
// 025's category table.
var Categories = []string{"coding", "files", "instructions", "threads"}

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
	// Dir is the task's directory.
	Dir string
	// Bundle is set when the starting files are a git bundle rather
	// than a fixture directory.
	Bundle bool
	// Check is the check.yaml source, placeholders unexpanded; empty
	// when the task's checker is check.sh.
	Check []byte
}

// Agent overrides the suite's agent for one task: the built-in tools it
// holds, its own instructions, and the subagents its threads may spawn.
type Agent struct {
	// Tools are the built-ins the agent holds; nil holds every one.
	Tools        []string            `yaml:"tools"`
	Instructions string              `yaml:"instructions"`
	Subagents    map[string]Subagent `yaml:"subagents"`
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
		Serve: f.Serve, Dir: dir,
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
	if t.Serve != "" {
		if fi, err := os.Stat(filepath.Join(dir, t.Serve)); err != nil || !fi.IsDir() {
			return fail("serve %q is not a directory of the task", t.Serve)
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

// checkTools refuses a tool list naming anything but a built-in.
func checkTools(a Agent) error {
	known := builtinNames()
	lists := map[string][]string{"agent": a.Tools}
	for name, s := range a.Subagents {
		lists["subagent "+name] = s.Tools
	}
	for who, list := range lists {
		for _, n := range list {
			if !slices.Contains(known, n) {
				return fmt.Errorf("the %s holds %q, which is not a built-in tool (%s)", who, n, strings.Join(known, ", "))
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
	b, yerr := os.ReadFile(filepath.Join(t.Dir, FileCheckYAML))
	_, serr := os.Stat(filepath.Join(t.Dir, FileCheckSh))
	switch {
	case yerr == nil && serr == nil:
		return fmt.Errorf("both %s and %s are present; a task has one checker", FileCheckYAML, FileCheckSh)
	case yerr != nil && serr != nil:
		return fmt.Errorf("no checker: write %s or %s", FileCheckYAML, FileCheckSh)
	case yerr != nil:
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
