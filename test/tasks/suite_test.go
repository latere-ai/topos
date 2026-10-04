// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
)

// suite loads the suite of this directory.
func suite(t *testing.T) []Task {
	t.Helper()
	all, err := Load(".")
	if err != nil {
		t.Fatal(err)
	}
	return all
}

// TestScriptedSolutions runs every task of the suite with its scripted
// solution, and every task that has one with its scripted wrong
// solution, through the runner and the harness in process. A solution
// passes, a wrong solution is refused by the checker itself (an
// instruction test's by an assertion on the log, not on the files),
// and a second evaluation of the same final directory and log gives
// the same verdict.
func TestScriptedSolutions(t *testing.T) {
	for _, task := range suite(t) {
		scripts := []string{FileSolution}
		if _, err := os.Stat(filepath.Join(task.Dir, FileWrong)); err == nil {
			scripts = append(scripts, FileWrong)
		}
		for _, script := range scripts {
			t.Run(task.ID+"/"+strings.TrimSuffix(script, ".yaml"), func(t *testing.T) {
				t.Parallel()
				ctx := t.Context()
				work := t.TempDir()
				res, err := RunTask(ctx, task, Options{Work: work, Script: script}, 1)
				if err != nil {
					t.Fatal(err)
				}
				if res.Verdict == nil {
					t.Fatalf("the checker never ran: %s", res.Reason)
				}
				if want := script == FileSolution; res.Passed != want {
					t.Fatalf("passed %v, want %v: %s\nlog %s", res.Passed, want, res.Reason, res.Log)
				}
				if script == FileWrong && task.Category == "instructions" && !slices.Contains(LogKinds, res.Verdict.Kind) {
					t.Fatalf("the wrong solution was refused by a %s assertion, not by one on the log: %s", res.Verdict.Kind, res.Verdict.Reason)
				}
				t.Logf("%s: steps %d, calls %d: %s", script, res.Steps, res.ToolCalls, res.Reason)
				again, err := Judge(ctx, task, res, filepath.Join(work, "again"))
				if err != nil {
					t.Fatal(err)
				}
				if again != *res.Verdict {
					t.Fatalf("two evaluations disagree: %+v then %+v", *res.Verdict, again)
				}
			})
		}
	}
}

// TestEveryTaskHasItsScripts holds the suite to its own rule: every
// task has a scripted solution, and every instruction test also has a
// scripted wrong solution.
func TestEveryTaskHasItsScripts(t *testing.T) {
	for _, task := range suite(t) {
		want := []string{FileSolution}
		if task.Category == "instructions" {
			want = append(want, FileWrong)
		}
		for _, f := range want {
			if _, err := os.Stat(filepath.Join(task.Dir, f)); err != nil {
				t.Errorf("%s has no %s", task.ID, f)
			}
		}
	}
}

// TestEveryToolDescriptionHasAnInstructionTest is spec 008's rule: each
// built-in tool's description, and the harness's question tool's, has
// an instruction test directory.
func TestEveryToolDescriptionHasAnInstructionTest(t *testing.T) {
	var names []string
	for _, b := range tools.Builtins() {
		names = append(names, b.Definition().Name)
	}
	for _, name := range slices.Concat(names, tools.OptIn(), []string{harness.ToolQuestion}) {
		task, err := LoadTask(filepath.Join("instructions", name))
		if err != nil {
			t.Errorf("the %s tool has no instruction test: %v", name, err)
			continue
		}
		if task.Category != "instructions" {
			t.Errorf("%s is in %s", task.ID, task.Category)
		}
	}
}

// TestFixturesAreModules keeps every Go file of a task out of this
// module: a fixture holding Go code is a module of its own, and the
// files a checker overlays sit under testdata, so neither go vet nor
// the main module's tests ever compile them.
func TestFixturesAreModules(t *testing.T) {
	for _, task := range suite(t) {
		err := filepath.WalkDir(task.Dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Ext(p) != ".go" {
				return err
			}
			for dir := filepath.Dir(p); dir != filepath.Dir(task.Dir); dir = filepath.Dir(dir) {
				if filepath.Base(dir) == "testdata" {
					return nil
				}
				if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
					return nil
				}
			}
			t.Errorf("%s is a Go file of the main module", p)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
