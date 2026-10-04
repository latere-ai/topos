// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/session"
)

// Check is a parsed check.yaml: assertions evaluated in order, the
// first failing one failing the run. Each assertion reads an outcome,
// the final working directory or the session's log, and none reads the
// model's prose but final_message, whose instruction's rule is what the
// final message states.
type Check []Assertion

// Assertion is one entry of check.yaml; exactly one field is set.
type Assertion struct {
	// Go runs the go command in a copy of the working directory and
	// passes on exit 0.
	Go *GoCheck `yaml:"go"`
	// File asserts on one file's content.
	File *FileCheck `yaml:"file"`
	// Unchanged are files that must equal the fixture's copy.
	Unchanged []string `yaml:"unchanged"`
	// Absent are paths that must not exist.
	Absent []string `yaml:"absent"`
	// OnlyFiles is the whole set of files the working directory holds,
	// .git aside: nothing else was created, and nothing was re-rooted.
	OnlyFiles []string `yaml:"only_files"`
	// NoMatch passes when no file matching a glob matches a pattern.
	NoMatch *NoMatch `yaml:"no_match"`
	// Call passes when the log holds calls that match, at least Min
	// (default 1) and at most Max.
	Call *CallCheck `yaml:"call"`
	// NoCall passes when the log holds no call that matches.
	NoCall *CallCheck `yaml:"no_call"`
	// Todo asserts on how the root thread kept its todo list.
	Todo *TodoCheck `yaml:"todo"`
	// Question asserts on the root thread's question calls.
	Question *QuestionCheck `yaml:"question"`
	// FinalMessage asserts on the text of the root thread's last
	// agent.message, for an instruction whose rule is what that message
	// states, such as the question tool's rule that an agent names what
	// it assumed.
	FinalMessage *FinalMessageCheck `yaml:"final_message"`
}

// QuestionCheck asserts on the root thread's question calls: there are
// Calls of them (default 1), every option of each has a description, an
// option marked recommended comes first in its question, and no
// question's text matches NotMatching, a pattern of what a question must
// not ask, such as permission for an action.
type QuestionCheck struct {
	Calls       *int   `yaml:"calls"`
	NotMatching string `yaml:"not_matching"`
}

// FinalMessageCheck asserts that the root thread's last agent.message
// matches a pattern.
type FinalMessageCheck struct {
	Matches string `yaml:"matches"`
}

// GoCheck runs go with Args in a copy of the working directory, with
// the files of the task's Overlay directory copied over it first: a
// test the model never saw.
type GoCheck struct {
	Args    []string `yaml:"args"`
	Overlay string   `yaml:"overlay"`
}

// FileCheck asserts on one file. Equals and EqualsFile compare with
// trailing white space removed from both sides; JSONEqualsFile compares
// the decoded values.
type FileCheck struct {
	Path           string   `yaml:"path"`
	Equals         *string  `yaml:"equals"`
	EqualsFile     string   `yaml:"equals_file"`
	JSONEqualsFile string   `yaml:"json_equals_file"`
	Contains       []string `yaml:"contains"`
	NotContains    []string `yaml:"not_contains"`
	Matches        string   `yaml:"matches"`
}

// NoMatch names the files by a glob relative to the working directory
// (** crosses directories) and a pattern none of them may match.
type NoMatch struct {
	Glob    string `yaml:"glob"`
	Pattern string `yaml:"pattern"`
}

// CallCheck selects tool calls from the log: an agent.tool_use joined
// with its tool.result.
type CallCheck struct {
	Tool string `yaml:"tool"`
	// Thread is root (the session's own thread), sub (any spawned
	// thread) or empty for any.
	Thread string `yaml:"thread"`
	// Outcome is the result's outcome; empty matches any.
	Outcome string             `yaml:"outcome"`
	Input   map[string]Matcher `yaml:"input"`
	Min     *int               `yaml:"min"`
	Max     *int               `yaml:"max"`
}

// Matcher asserts on one top-level field of a call's input. Every set
// condition must hold.
type Matcher struct {
	Present   *bool  `yaml:"present"`
	Equals    any    `yaml:"equals"`
	NotEquals any    `yaml:"not_equals"`
	Matches   string `yaml:"matches"`
	Contains  string `yaml:"contains"`
	MinLength int    `yaml:"min_length"`
}

// TodoCheck asserts on the root thread's todo calls: at least one list
// held MinItems items, no list had more than MaxInProgress items in
// progress, and every item of the last list has the status Final.
type TodoCheck struct {
	MinItems      int    `yaml:"min_items"`
	MaxInProgress int    `yaml:"max_in_progress"`
	Final         string `yaml:"final"`
}

// Kind names the assertion's one field.
func (a Assertion) Kind() string {
	kinds := a.kinds()
	if len(kinds) == 1 {
		return kinds[0]
	}
	return ""
}

func (a Assertion) kinds() []string {
	var out []string
	for _, k := range []struct {
		name string
		set  bool
	}{
		{"go", a.Go != nil}, {"file", a.File != nil}, {"unchanged", a.Unchanged != nil},
		{"absent", a.Absent != nil}, {"only_files", a.OnlyFiles != nil}, {"no_match", a.NoMatch != nil},
		{"call", a.Call != nil}, {"no_call", a.NoCall != nil}, {"todo", a.Todo != nil},
		{"question", a.Question != nil}, {"final_message", a.FinalMessage != nil},
	} {
		if k.set {
			out = append(out, k.name)
		}
	}
	return out
}

// LogKinds are the assertions that read the session's log.
var LogKinds = []string{"call", "no_call", "todo", "question", "final_message"}

// ParseCheck reads a check.yaml and refuses an assertion with no kind or
// with two, an unknown field, a bad pattern, and a call with no tool or
// an unknown thread.
func ParseCheck(b []byte) (Check, error) {
	var c Check
	if err := yaml.UnmarshalWithOptions(b, &c, yaml.DisallowUnknownField()); err != nil {
		return nil, fmt.Errorf("tasks: %s: %w", FileCheckYAML, err)
	}
	if len(c) == 0 {
		return nil, fmt.Errorf("tasks: %s holds no assertion", FileCheckYAML)
	}
	for i, a := range c {
		if err := a.validate(); err != nil {
			return nil, fmt.Errorf("tasks: %s: assertion %d: %w", FileCheckYAML, i+1, err)
		}
	}
	return c, nil
}

func (a Assertion) validate() error {
	if k := a.kinds(); len(k) != 1 {
		return fmt.Errorf("sets %d kinds %v; an assertion is exactly one of go, file, unchanged, absent, only_files, no_match, call, no_call, todo, question, final_message", len(k), k)
	}
	var patterns []string
	switch {
	case a.Go != nil && len(a.Go.Args) == 0:
		return errors.New("go has no args")
	case a.File != nil && a.File.Path == "":
		return errors.New("file has no path")
	case a.File != nil:
		patterns = append(patterns, a.File.Matches)
	case a.NoMatch != nil && (a.NoMatch.Glob == "" || a.NoMatch.Pattern == ""):
		return errors.New("no_match needs a glob and a pattern")
	case a.NoMatch != nil:
		patterns = append(patterns, a.NoMatch.Pattern)
	case a.Todo != nil && a.Todo.Final != "" && !slices.Contains(todoStatuses, a.Todo.Final):
		return fmt.Errorf("todo final %q is not one of %v", a.Todo.Final, todoStatuses)
	case a.Question != nil:
		patterns = append(patterns, a.Question.NotMatching)
	case a.FinalMessage != nil && a.FinalMessage.Matches == "":
		return errors.New("final_message has no pattern")
	case a.FinalMessage != nil:
		patterns = append(patterns, a.FinalMessage.Matches)
	}
	for _, c := range []*CallCheck{a.Call, a.NoCall} {
		if c == nil {
			continue
		}
		if c.Tool == "" {
			return errors.New("a call names no tool")
		}
		if c.Thread != "" && c.Thread != "root" && c.Thread != "sub" {
			return fmt.Errorf("thread %q is not root, sub or empty", c.Thread)
		}
		for _, m := range c.Input {
			patterns = append(patterns, m.Matches)
		}
	}
	for _, p := range patterns {
		if _, err := regexp.Compile(p); err != nil {
			return err
		}
	}
	return nil
}

var todoStatuses = []string{"pending", "in_progress", "completed"}

// Input is what a checker judges.
type Input struct {
	// Workdir is the final working directory.
	Workdir string
	// Task is the task whose checker runs.
	Task Task
	// Log is the path of the session's events.jsonl, and Events its
	// events.
	Log    string
	Events []session.Event
	// Scratch is a directory the checker may write, never the working
	// directory.
	Scratch string
	// ServeURL is the base URL of the task's served pages, if any.
	ServeURL string
}

// Verdict is a checker's answer. A failing verdict names the
// assertion, counted from 1, and its kind.
type Verdict struct {
	Pass      bool   `json:"pass"`
	Assertion int    `json:"assertion,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// Evaluate runs the task's checker over the input. An error is a
// checker that could not run at all; a failing check is a verdict.
func Evaluate(ctx context.Context, in Input) (Verdict, error) {
	if in.Task.Check == nil {
		return checkScript(ctx, in)
	}
	c, err := ParseCheck(expand(in.Task.Check, in.Workdir, in.ServeURL))
	if err != nil {
		return Verdict{}, err
	}
	calls := Calls(in.Events)
	for i, a := range c {
		reason, err := a.eval(ctx, in, calls, i)
		if err != nil {
			return Verdict{}, fmt.Errorf("tasks: %s assertion %d: %w", in.Task.ID, i+1, err)
		}
		if reason != "" {
			return Verdict{Assertion: i + 1, Kind: a.Kind(), Reason: reason}, nil
		}
	}
	return Verdict{Pass: true}, nil
}

// eval returns the reason the assertion fails, or "" when it holds.
func (a Assertion) eval(ctx context.Context, in Input, calls []Call, i int) (string, error) {
	switch {
	case a.Go != nil:
		return a.Go.eval(ctx, in, i)
	case a.File != nil:
		return a.File.eval(in)
	case a.Unchanged != nil:
		return unchanged(in, a.Unchanged)
	case a.Absent != nil:
		return absent(in, a.Absent), nil
	case a.OnlyFiles != nil:
		return onlyFiles(in, a.OnlyFiles)
	case a.NoMatch != nil:
		return a.NoMatch.eval(in)
	case a.Call != nil:
		return a.Call.eval(calls, 1), nil
	case a.NoCall != nil:
		c := *a.NoCall
		zero := 0
		c.Max = &zero
		return c.eval(calls, 0), nil
	case a.Question != nil:
		return a.Question.eval(calls), nil
	case a.FinalMessage != nil:
		return a.FinalMessage.eval(in.Events), nil
	}
	return a.Todo.eval(calls), nil
}

func (q *QuestionCheck) eval(calls []Call) string {
	want := 1
	if q.Calls != nil {
		want = *q.Calls
	}
	var asked []Call
	for _, c := range calls {
		if c.Name == session.ToolQuestion && c.Thread == "" {
			asked = append(asked, c)
		}
	}
	if len(asked) != want {
		return fmt.Sprintf("%d question calls, want %d", len(asked), want)
	}
	for i, c := range asked {
		var in session.QuestionInput
		if err := json.Unmarshal(c.Input, &in); err != nil {
			return fmt.Sprintf("question call %d: the input does not decode: %v", i+1, err)
		}
		for j, question := range in.Questions {
			if q.NotMatching != "" && regexp.MustCompile(q.NotMatching).MatchString(question.Question) {
				return fmt.Sprintf("question call %d asks %q, which matches %s", i+1, question.Question, q.NotMatching)
			}
			for k, o := range question.Options {
				if strings.TrimSpace(o.Description) == "" {
					return fmt.Sprintf("question call %d: option %q of question %d has no description", i+1, o.Label, j+1)
				}
				if o.Recommended && k != 0 {
					return fmt.Sprintf("question call %d: the recommended option %q of question %d is not first", i+1, o.Label, j+1)
				}
			}
		}
	}
	return ""
}

func (f *FinalMessageCheck) eval(evs []session.Event) string {
	text := ""
	for _, e := range evs {
		var m session.AgentMessage
		if e.Type != session.TypeAgentMessage || e.Thread != "" || e.Redacted() || e.Decode(&m) != nil {
			continue
		}
		var parts []string
		for _, b := range m.Message.Blocks {
			if b.Type == ir.BlockText {
				parts = append(parts, b.Text)
			}
		}
		if len(parts) > 0 {
			text = strings.Join(parts, "\n")
		}
	}
	if !regexp.MustCompile(f.Matches).MatchString(text) {
		return fmt.Sprintf("the final message %q does not match %s", clip(text), f.Matches)
	}
	return ""
}

// path makes a checked path absolute against the working directory.
func (in Input) path(p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(in.Workdir, filepath.FromSlash(p))
}

func (g *GoCheck) eval(ctx context.Context, in Input, i int) (string, error) {
	dir := filepath.Join(in.Scratch, fmt.Sprintf("go-%d", i+1))
	if err := copyTree(in.Workdir, dir); err != nil {
		return "", err
	}
	if g.Overlay != "" {
		if err := copyTree(filepath.Join(in.Task.Dir, g.Overlay), dir); err != nil {
			return "", err
		}
	}
	env, err := withTempDir(os.Environ(), filepath.Join(in.Scratch, fmt.Sprintf("tmp-%d", i+1)))
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "go", g.Args...)
	cmd.Dir = dir
	cmd.Env = append(env, "GOWORK=off", "GOTOOLCHAIN=local", "GOFLAGS=")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		return fmt.Sprintf("go %s exited %d:\n%s", strings.Join(g.Args, " "), ee.ExitCode(), tail(out, 40)), nil
	case err != nil:
		return "", fmt.Errorf("run go: %w", err)
	}
	return "", nil
}

// goTimes are the durations and cache marks go test prints, which
// differ between two runs of the same test.
var goTimes = regexp.MustCompile(`(?m)(\t(\(cached\)|[0-9.]+s)| \([0-9.]+s\))$`)

// tail is the last n lines of out, with go test's durations removed so
// that one outcome always reads the same.
func tail(out []byte, n int) string {
	lines := strings.Split(strings.TrimRight(goTimes.ReplaceAllString(string(out), ""), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func (f *FileCheck) eval(in Input) (string, error) {
	b, err := os.ReadFile(in.path(f.Path))
	if errors.Is(err, fs.ErrNotExist) {
		return f.Path + " does not exist", nil
	}
	if err != nil {
		return "", err
	}
	got := string(b)
	trimmed := strings.TrimRight(got, " \t\r\n")
	if f.Equals != nil && trimmed != strings.TrimRight(*f.Equals, " \t\r\n") {
		return fmt.Sprintf("%s is %q, want %q", f.Path, clip(trimmed), clip(*f.Equals)), nil
	}
	if f.EqualsFile != "" {
		want, err := os.ReadFile(filepath.Join(in.Task.Dir, f.EqualsFile))
		if err != nil {
			return "", err
		}
		if trimmed != strings.TrimRight(string(want), " \t\r\n") {
			return fmt.Sprintf("%s differs from %s", f.Path, f.EqualsFile), nil
		}
	}
	if f.JSONEqualsFile != "" {
		want, err := os.ReadFile(filepath.Join(in.Task.Dir, f.JSONEqualsFile))
		if err != nil {
			return "", err
		}
		var g, w any
		if err := json.Unmarshal(want, &w); err != nil {
			return "", fmt.Errorf("%s: %w", f.JSONEqualsFile, err)
		}
		if err := json.Unmarshal(b, &g); err != nil {
			return fmt.Sprintf("%s is not JSON: %v", f.Path, err), nil
		}
		if !reflect.DeepEqual(g, w) {
			return fmt.Sprintf("%s does not hold the JSON of %s", f.Path, f.JSONEqualsFile), nil
		}
	}
	for _, s := range f.Contains {
		if !strings.Contains(got, s) {
			return fmt.Sprintf("%s does not contain %q", f.Path, clip(s)), nil
		}
	}
	for _, s := range f.NotContains {
		if strings.Contains(got, s) {
			return fmt.Sprintf("%s contains %q", f.Path, clip(s)), nil
		}
	}
	if f.Matches != "" && !regexp.MustCompile(f.Matches).MatchString(got) {
		return fmt.Sprintf("%s does not match %s", f.Path, f.Matches), nil
	}
	return "", nil
}

// clip shortens a string for a reason.
func clip(s string) string {
	const n = 200
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func unchanged(in Input, paths []string) (string, error) {
	if in.Task.Bundle {
		return "", errors.New("unchanged compares against a fixture directory, and this task starts from a bundle")
	}
	for _, p := range paths {
		want, err := os.ReadFile(filepath.Join(in.Task.Dir, DirFixture, filepath.FromSlash(p)))
		if err != nil {
			return "", err
		}
		got, err := os.ReadFile(in.path(p))
		if errors.Is(err, fs.ErrNotExist) {
			return p + " was removed", nil
		}
		if err != nil {
			return "", err
		}
		if !bytes.Equal(got, want) {
			return p + " changed", nil
		}
	}
	return "", nil
}

func absent(in Input, paths []string) string {
	for _, p := range paths {
		if _, err := os.Lstat(in.path(p)); err == nil {
			return p + " exists"
		}
	}
	return ""
}

// files lists the regular files under dir as sorted slash paths, .git
// aside.
func files(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	slices.Sort(out)
	return out, err
}

func onlyFiles(in Input, want []string) (string, error) {
	got, err := files(in.Workdir)
	if err != nil {
		return "", err
	}
	w := slices.Sorted(slices.Values(want))
	var extra, missing []string
	for _, g := range got {
		if !slices.Contains(w, g) {
			extra = append(extra, g)
		}
	}
	for _, x := range w {
		if !slices.Contains(got, x) {
			missing = append(missing, x)
		}
	}
	if len(extra) == 0 && len(missing) == 0 {
		return "", nil
	}
	return fmt.Sprintf("the working directory holds %v beyond the expected files and lacks %v", extra, missing), nil
}

func (n *NoMatch) eval(in Input) (string, error) {
	all, err := files(in.Workdir)
	if err != nil {
		return "", err
	}
	g, re := globRegexp(n.Glob), regexp.MustCompile(n.Pattern)
	for _, p := range all {
		if !g.MatchString(p) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(in.Workdir, filepath.FromSlash(p)))
		if err != nil {
			return "", err
		}
		if loc := re.FindIndex(b); loc != nil {
			line := bytes.Count(b[:loc[0]], []byte("\n")) + 1
			return fmt.Sprintf("%s:%d matches %s", p, line, n.Pattern), nil
		}
	}
	return "", nil
}

// globRegexp compiles a path glob: ** matches any number of whole
// segments, * and ? stay within one.
func globRegexp(glob string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		switch {
		case strings.HasPrefix(glob[i:], "**/"):
			b.WriteString("(.*/)?")
			i += 2
		case strings.HasPrefix(glob[i:], "**"):
			b.WriteString(".*")
			i++
		case glob[i] == '*':
			b.WriteString("[^/]*")
		case glob[i] == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(glob[i : i+1]))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// Call is one tool call of the log: the agent.tool_use and, when it has
// one, its tool.result.
type Call struct {
	Thread  string
	ID      string
	Name    string
	Input   json.RawMessage
	Outcome string
	Text    string
}

// Calls joins every agent.tool_use of the log with its tool.result, in
// the log's order.
func Calls(evs []session.Event) []Call {
	var out []Call
	at := map[string]int{}
	for _, e := range evs {
		if e.Redacted() {
			continue
		}
		switch e.Type {
		case session.TypeAgentToolUse:
			var u session.AgentToolUse
			if e.Decode(&u) != nil {
				continue
			}
			at[e.Thread+"\x00"+u.ToolUseID] = len(out)
			out = append(out, Call{Thread: e.Thread, ID: u.ToolUseID, Name: u.Name, Input: u.Input})
		case session.TypeToolResult:
			var r session.ToolResult
			if e.Decode(&r) != nil {
				continue
			}
			i, ok := at[e.Thread+"\x00"+r.ToolUseID]
			if !ok {
				continue
			}
			out[i].Outcome = r.Outcome
			var text []string
			for _, b := range r.Content {
				if b.Type == ir.BlockText {
					text = append(text, b.Text)
				}
			}
			out[i].Text = strings.Join(text, "\n")
		}
	}
	return out
}

// matches reports whether a call is selected.
func (c *CallCheck) matches(call Call) bool {
	if call.Name != c.Tool || (c.Outcome != "" && call.Outcome != c.Outcome) {
		return false
	}
	if (c.Thread == "root" && call.Thread != "") || (c.Thread == "sub" && call.Thread == "") {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(call.Input, &fields) != nil {
		return len(c.Input) == 0
	}
	for name, m := range c.Input {
		raw, present := fields[name]
		if !m.holds(raw, present) {
			return false
		}
	}
	return true
}

// holds applies a matcher to one input field.
func (m Matcher) holds(raw json.RawMessage, present bool) bool {
	if m.Present != nil && *m.Present != present {
		return false
	}
	if m.NotEquals != nil && present && sameJSON(raw, m.NotEquals) {
		return false
	}
	needsValue := m.Equals != nil || m.Matches != "" || m.Contains != "" || m.MinLength > 0
	if !needsValue {
		return true
	}
	if !present {
		return false
	}
	if m.Equals != nil && !sameJSON(raw, m.Equals) {
		return false
	}
	var s string
	isString := json.Unmarshal(raw, &s) == nil
	if (m.Matches != "" || m.Contains != "" || m.MinLength > 0) && !isString {
		return false
	}
	return (m.Matches == "" || regexp.MustCompile(m.Matches).MatchString(s)) &&
		strings.Contains(s, m.Contains) && len(s) >= m.MinLength
}

// sameJSON compares a JSON value with a YAML value by their JSON forms.
func sameJSON(raw json.RawMessage, v any) bool {
	var got any
	if json.Unmarshal(raw, &got) != nil {
		return false
	}
	g, gerr := json.Marshal(got)
	w, werr := json.Marshal(v)
	return gerr == nil && werr == nil && bytes.Equal(g, w)
}

func (c *CallCheck) eval(calls []Call, minDefault int) string {
	n := 0
	for _, call := range calls {
		if c.matches(call) {
			n++
		}
	}
	lo := minDefault
	if c.Min != nil {
		lo = *c.Min
	}
	if n < lo {
		return fmt.Sprintf("%d %s call(s) match, want at least %d", n, c.Tool, lo)
	}
	if c.Max != nil && n > *c.Max {
		return fmt.Sprintf("%d %s call(s) match, want at most %d", n, c.Tool, *c.Max)
	}
	return ""
}

type todoItem struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"`
}

func (t *TodoCheck) eval(calls []Call) string {
	var lists [][]todoItem
	for _, c := range calls {
		if c.Name != "todo" || c.Thread != "" || c.Outcome != "ok" {
			continue
		}
		var in struct {
			Todos []todoItem `json:"todos"`
		}
		if json.Unmarshal(c.Input, &in) == nil {
			lists = append(lists, in.Todos)
		}
	}
	if len(lists) == 0 {
		return "the root thread kept no todo list"
	}
	most := 0
	for i, l := range lists {
		most = max(most, len(l))
		busy := 0
		for _, it := range l {
			if it.Status == "in_progress" {
				busy++
			}
		}
		if t.MaxInProgress > 0 && busy > t.MaxInProgress {
			return fmt.Sprintf("todo call %d has %d items in progress, want at most %d", i+1, busy, t.MaxInProgress)
		}
	}
	if most < t.MinItems {
		return fmt.Sprintf("the longest todo list has %d items, want at least %d", most, t.MinItems)
	}
	if t.Final != "" {
		for _, it := range lists[len(lists)-1] {
			if it.Status != t.Final {
				return fmt.Sprintf("the last todo list leaves %q %s, want %s", it.ID, it.Status, t.Final)
			}
		}
	}
	return ""
}

// checkScript runs check.sh with the working directory and the log's
// path as its arguments, in the working directory; exit 0 passes.
func checkScript(ctx context.Context, in Input) (Verdict, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", filepath.Join(in.Task.Dir, FileCheckSh), in.Workdir, in.Log)
	cmd.Dir = in.Workdir
	cmd.Env = append(os.Environ(), "TOPOS_TASK="+in.Task.ID, "TOPOS_SERVE_URL="+in.ServeURL)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		return Verdict{Assertion: 1, Kind: "check.sh", Reason: fmt.Sprintf("check.sh exited %d:\n%s", ee.ExitCode(), tail(out, 40))}, nil
	case err != nil:
		return Verdict{}, fmt.Errorf("tasks: %s: run check.sh: %w", in.Task.ID, err)
	}
	return Verdict{Pass: true}, nil
}

// copyTree copies the regular files and directories under src into dst,
// keeping each file's mode.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case !fi.Mode().IsRegular():
			return fmt.Errorf("tasks: %s is not a regular file", p)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, fi.Mode().Perm())
	})
}
