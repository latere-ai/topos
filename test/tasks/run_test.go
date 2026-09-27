// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"latere.ai/x/pkg/llmdialect/bridge"
	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// synthetic is the suite of testdata/suite: coding/echo, two runs with
// a maxCost of $0.01, and files/shell, one run checked by check.sh.
const synthetic = "testdata/suite"

func loadTask(t *testing.T, id string) Task {
	t.Helper()
	task, err := LoadTask(filepath.Join(synthetic, filepath.FromSlash(id)))
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestRunReportsEveryRun(t *testing.T) {
	var seen atomic.Int32
	rep, err := Run(t.Context(), Options{
		Dir: synthetic, Work: t.TempDir(), Script: FileSolution, Commit: "abc123",
		OnRun: func(RunResult) { seen.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total != 3 || rep.Passed != 3 || rep.PassRate != 1 || rep.Incomplete || seen.Load() != 3 {
		t.Fatalf("report %+v", rep)
	}
	if rep.Model != "scripted:solution.yaml" || rep.Commit != "abc123" || rep.SpendUSDMicro != 6000 || rep.Started.IsZero() {
		t.Fatalf("report %+v", rep)
	}
	if len(rep.Tasks) != 2 || rep.Tasks[0].Task != "coding/echo" || rep.Tasks[0].Runs != 2 || rep.Tasks[0].MedianSteps != 2 || rep.Tasks[0].MedianCostUSDMicro != 3000 {
		t.Fatalf("summary %+v", rep.Tasks)
	}
	for _, r := range rep.Runs {
		if r.Session == "" || r.Log == "" || r.Workdir == "" || r.Stop != session.StopCompleted || r.Verdict == nil || !r.Verdict.Pass {
			t.Fatalf("run %+v", r)
		}
		if _, err := os.Stat(r.Log); err != nil {
			t.Fatalf("the session's log is not kept: %v", err)
		}
	}
}

func TestRunCountsFailedRuns(t *testing.T) {
	rep, err := Run(t.Context(), Options{Dir: synthetic, Work: t.TempDir(), Script: FileWrong})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total != 3 || rep.Passed != 0 || rep.PassRate != 0 {
		t.Fatalf("report %+v", rep)
	}
	if r := rep.Runs[0]; !strings.Contains(r.Reason, "the checker failed at file assertion 1") {
		t.Fatalf("echo's reason %q", r.Reason)
	}
	if r := rep.Runs[2]; r.Task != "files/shell" || !strings.Contains(r.Reason, "check.sh exited 1") {
		t.Fatalf("shell's reason %q", r.Reason)
	}
}

func TestRunFiltersAndOverridesTheRuns(t *testing.T) {
	rep, err := Run(t.Context(), Options{Dir: synthetic, Work: t.TempDir(), Script: FileSolution, Filter: "^files/", Runs: 3})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total != 3 || len(rep.Tasks) != 1 || rep.Tasks[0].Task != "files/shell" {
		t.Fatalf("report %+v", rep)
	}
	for _, f := range []string{"(", "^nothing/"} {
		if _, err := Run(t.Context(), Options{Dir: synthetic, Work: t.TempDir(), Filter: f}); err == nil {
			t.Errorf("filter %q ran", f)
		}
	}
	if _, err := Run(t.Context(), Options{Dir: t.TempDir(), Work: t.TempDir()}); err == nil {
		t.Error("an empty suite ran")
	}
}

func TestRunRefusesItsWorkDirectory(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.CommandContext(t.Context(), "git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	for _, w := range []string{"", filepath.Join(t.TempDir(), "missing"), repo} {
		_, err := Run(t.Context(), Options{Dir: synthetic, Work: w, Script: FileSolution})
		if err == nil {
			t.Errorf("work directory %q ran", w)
		}
	}
}

func TestSpendCapsFailClosed(t *testing.T) {
	ctx := t.Context()
	echo := loadTask(t, "coding/echo")
	t.Run("a run at its maxCost stops with budget", func(t *testing.T) {
		res, err := RunTask(ctx, echo, Options{Work: t.TempDir(), Script: "costly.yaml"}, 1)
		if err != nil {
			t.Fatal(err)
		}
		if res.Passed || res.Stop != session.StopBudget || res.CostUSDMicro != 10_000 || res.Verdict != nil {
			t.Fatalf("run %+v", res)
		}
	})
	t.Run("a run past its maxCost fails", func(t *testing.T) {
		res, err := RunTask(ctx, echo, Options{Work: t.TempDir(), Script: "overspend.yaml"}, 1)
		if err != nil {
			t.Fatal(err)
		}
		if res.Passed || !strings.Contains(res.Reason, "past its maxCost of $0.0100") || res.Verdict != nil {
			t.Fatalf("run %+v", res)
		}
	})
	t.Run("a suite past its budget is incomplete", func(t *testing.T) {
		rep, err := Run(ctx, Options{Dir: synthetic, Work: t.TempDir(), Script: FileSolution, BudgetUSDMicro: 2500})
		if err != nil {
			t.Fatal(err)
		}
		if !rep.Incomplete || rep.Total != 1 || !strings.Contains(rep.IncompleteReason, "budget of $0.0025") {
			t.Fatalf("report %+v", rep)
		}
		gating, err := (Bar{Model: BarModel{Name: rep.Model}, Threshold: ptr(0.5)}).Gate(rep)
		if !gating || err == nil {
			t.Fatalf("an incomplete run passed the bar: %v %v", gating, err)
		}
	})
	t.Run("a key refused for spend ends the suite incomplete", func(t *testing.T) {
		stub := luxstub.New(t)
		stub.Script(httpModel, luxstub.Reply{Fail: &luxstub.Failure{
			Status: 429, RetryAfter: "3600", Times: 10,
			Body: `{"type":"error","error":{"type":"budget_exhausted","message":"the key's budget is spent"}}`,
		}})
		rep, err := Run(ctx, Options{Dir: synthetic, Work: t.TempDir(), Filter: "coding/echo", Connection: stubConnection(stub)})
		if err != nil {
			t.Fatal(err)
		}
		if !rep.Incomplete || rep.Total != 1 || !strings.Contains(rep.IncompleteReason, "budget_exhausted") {
			t.Fatalf("report %+v", rep)
		}
		if r := rep.Runs[0]; r.Passed || !r.BudgetExhausted || r.Stop != session.StopBudget {
			t.Fatalf("run %+v", r)
		}
	})
}

func ptr[T any](v T) *T { return &v }

// httpModel is a catalog model the stub Lux answers for.
const httpModel = "claude-haiku-4-5"

func stubConnection(stub *luxstub.Server) models.Connection {
	return models.Connection{BaseURL: stub.URL() + "/anthropic", Model: httpModel, Credential: "sk-test"}
}

func reply(blocks ...ir.Block) luxstub.Reply {
	stop := ir.StopEndTurn
	for _, b := range blocks {
		if b.Type == ir.BlockToolUse {
			stop = ir.StopToolUse
		}
	}
	return luxstub.Reply{Response: ir.Response{Model: httpModel, Blocks: blocks, StopReason: stop, Usage: ir.Usage{InputTokens: 1000, OutputTokens: 100}}}
}

// TestARunThroughTheHTTPModel drives a task through models/dialect
// against the stub Lux, the path a run against a real model takes.
func TestARunThroughTheHTTPModel(t *testing.T) {
	stub := luxstub.New(t)
	stub.Script(httpModel,
		reply(ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "toolu_1", Name: "write", Args: json.RawMessage(`{"path":"hello.txt","content":"hello"}`)}}),
		reply(ir.Block{Type: ir.BlockText, Text: "Done."}),
	)
	res, err := RunTask(t.Context(), loadTask(t, "coding/echo"), Options{Work: t.TempDir(), Connection: stubConnection(stub)}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed || res.Steps != 2 || res.ToolCalls != 1 || res.CostUSDMicro != 3000 {
		t.Fatalf("run %+v", res)
	}
	reqs := stub.Requests()
	if len(reqs) != 2 || reqs[0].Header.Get("x-api-key") != "sk-test" || len(reqs[0].Request.Tools) != 8 {
		t.Fatalf("requests %+v", reqs)
	}
	var prompt string
	for _, b := range reqs[0].Request.Messages[0].Blocks {
		prompt += b.Text
	}
	if !strings.Contains(prompt, "Write hello to hello.txt in "+res.Workdir) {
		t.Fatalf("the prompt's placeholder was not replaced: %q", prompt)
	}
}

func TestAModelTheCatalogDoesNotKnow(t *testing.T) {
	stub := luxstub.New(t)
	conn := stubConnection(stub)
	conn.Model = "house-model"
	rep, err := Run(t.Context(), Options{Dir: synthetic, Work: t.TempDir(), Filter: "coding/echo", Runs: 1, Connection: conn})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Passed != 0 || !strings.Contains(rep.Runs[0].Reason, "the suite could not run it") {
		t.Fatalf("report %+v", rep)
	}
	stub.Script("house-model",
		reply(ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "toolu_1", Name: "write", Args: json.RawMessage(`{"path":"hello.txt","content":"hello"}`)}}),
		reply(ir.Block{Type: ir.BlockText, Text: "Done."}),
	)
	entry := models.Entry{Family: models.FamilyAnthropic, InputWindow: 100_000, MaxOutputTokens: 8_000, Pricing: ScriptedEntry("x").Pricing}
	res, err := RunTask(t.Context(), loadTask(t, "coding/echo"), Options{Work: t.TempDir(), Connection: conn, Entry: &entry}, 1)
	if err != nil || !res.Passed {
		t.Fatalf("run %+v, %v", res, err)
	}
}

// TestARunTakesTheDoorsFigures: a run through a Lux door is priced and
// sized by the figures the door's model list gives, before the
// catalog's, and a door that does not answer the list fails the run.
func TestARunTakesTheDoorsFigures(t *testing.T) {
	stub := luxstub.New(t)
	stub.Models(bridge.Model{Name: httpModel, ContextWindow: 100_000, MaxOutputTokens: 2_048, Pricing: &bridge.ModelPricing{Currency: "USD", Input: "2", Output: "2"}})
	stub.Script(httpModel,
		reply(ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "toolu_1", Name: "write", Args: json.RawMessage(`{"path":"hello.txt","content":"hello"}`)}}),
		reply(ir.Block{Type: ir.BlockText, Text: "Done."}),
	)
	res, err := RunTask(t.Context(), loadTask(t, "coding/echo"), Options{Work: t.TempDir(), Connection: stubConnection(stub)}, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Two requests of 1000 input and 100 output tokens at 2 USD per
	// million each way, where the catalog's prices come to 3000.
	if !res.Passed || res.CostUSDMicro != 4_400 {
		t.Fatalf("run %+v", res)
	}
	if reqs := stub.Requests(); len(reqs) != 2 || reqs[0].Request.MaxTokens == nil || *reqs[0].Request.MaxTokens != 2_048 {
		t.Fatalf("requests %+v", reqs)
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	conn := stubConnection(stub)
	conn.BaseURL = gone.URL + "/anthropic"
	if _, err := RunTask(t.Context(), loadTask(t, "coding/echo"), Options{Work: t.TempDir(), Connection: conn}, 1); err == nil {
		t.Fatal("a run through a door that does not answer its model list")
	}
}

func TestAStopOtherThanTheEndOfTheTurnFails(t *testing.T) {
	res, err := RunTask(t.Context(), loadTask(t, "coding/echo"), Options{Work: t.TempDir(), Script: "exhausted.yaml"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed || res.Stop != session.StopError || res.Detail != "model_error" || !strings.Contains(res.Reason, "no step left") || res.BudgetExhausted {
		t.Fatalf("run %+v", res)
	}
}

// countingMachine is a host machine whose opener counts its openings.
type countingMachine struct {
	machine.Machine
}

func TestTheMachineOption(t *testing.T) {
	var opened atomic.Int32
	o := Options{Work: t.TempDir(), Script: FileSolution, Machine: func(ctx context.Context, m MachineOptions) (machine.Machine, error) {
		opened.Add(1)
		if m.TempDir == "" || m.Workdir == "" || m.SpillDir == "" || m.ID == "" {
			t.Errorf("machine options %+v", m)
		}
		h, err := openHost(ctx, m)
		return countingMachine{h}, err
	}}
	res, err := RunTask(t.Context(), loadTask(t, "coding/echo"), o, 1)
	if err != nil || !res.Passed || opened.Load() != 1 {
		t.Fatalf("run %+v, %v, opened %d", res, err, opened.Load())
	}
	o.Machine = func(context.Context, MachineOptions) (machine.Machine, error) {
		return nil, errors.New("no machine today")
	}
	res, err = RunTask(t.Context(), loadTask(t, "coding/echo"), o, 2)
	if err != nil || res.Passed || !strings.Contains(res.Reason, "no machine today") {
		t.Fatalf("run %+v, %v", res, err)
	}
}

func TestABundleIsCloned(t *testing.T) {
	ctx := t.Context()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	src := t.TempDir()
	writeFiles(t, src, map[string]string{"start.txt": "from the bundle\n"})
	git(src, "init", "-q")
	git(src, "add", ".")
	git(src, "commit", "-q", "-m", "start")
	suite := t.TempDir()
	dir := filepath.Join(suite, "coding", "bundled")
	writeFiles(t, dir, map[string]string{
		"task.yaml":     "name: bundled\ncategory: coding\nprompt: Write hello to hello.txt.\n",
		"check.yaml":    "- file: {path: hello.txt, equals: hello}\n- file: {path: start.txt, equals: from the bundle}\n",
		"solution.yaml": "steps:\n  - tool_calls:\n      - name: write\n        input: {path: hello.txt, content: hello}\n  - text: Done.\n",
	})
	git(src, "bundle", "create", filepath.Join(dir, FileBundle), "HEAD")
	task, err := LoadTask(dir)
	if err != nil || !task.Bundle {
		t.Fatalf("task %+v, %v", task, err)
	}
	res, err := RunTask(ctx, task, Options{Work: t.TempDir(), Script: FileSolution}, 1)
	if err != nil || !res.Passed {
		t.Fatalf("run %+v, %v", res, err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileBundle), []byte("not a bundle"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RunTask(ctx, task, Options{Work: t.TempDir(), Script: FileSolution}, 2); err == nil {
		t.Fatal("a broken bundle ran")
	}
}

func TestARunThatCannotStart(t *testing.T) {
	echo := loadTask(t, "coding/echo")
	if _, err := RunTask(t.Context(), echo, Options{Work: filepath.Join(t.TempDir(), "missing")}, 1); err == nil {
		t.Fatal("a run with no work directory ran")
	}
	if _, err := Judge(t.Context(), echo, RunResult{}, t.TempDir()); err == nil {
		t.Fatal("a run with no log was judged")
	}
	res, err := RunTask(t.Context(), echo, Options{Work: t.TempDir(), Script: FileSolution}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Judge(t.Context(), echo, res, filepath.Join(t.TempDir(), "no", "parent")); err == nil {
		t.Fatal("a scratch directory that cannot be made was used")
	}
	res.Log = filepath.Join(t.TempDir(), "sessions", res.Session, "events.jsonl")
	if _, err := Judge(t.Context(), echo, res, filepath.Join(t.TempDir(), "scratch")); err == nil {
		t.Fatal("a log that is gone was judged")
	}
}

func TestRunEndsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	o := Options{Dir: synthetic, Work: t.TempDir(), Script: FileSolution, OnRun: func(RunResult) { cancel() }}
	rep, err := Run(ctx, o)
	if !errors.Is(err, context.Canceled) || rep.Total != 1 {
		t.Fatalf("report %+v, %v", rep, err)
	}
}

func TestTheSuiteDefaultsToItsSourceDirectory(t *testing.T) {
	rep, err := Run(t.Context(), Options{Work: t.TempDir(), Script: FileSolution, Filter: "^files/abspaths$", Runs: 1})
	if err != nil || rep.Total != 1 || !rep.Runs[0].Passed {
		t.Fatalf("report %+v, %v", rep, err)
	}
}
