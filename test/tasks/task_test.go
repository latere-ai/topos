// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFiles writes files under root, their parents created.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const minimalCheck = "- absent: [nothing]\n"

func TestLoadAppliesTheDefaults(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"coding/plain/task.yaml":  "name: plain\ncategory: coding\nprompt: |\n  Do it.\n",
		"coding/plain/check.yaml": minimalCheck,
		"coding/notes.txt":        "a file beside the task directories is not a task",
	})
	all, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("loaded %d tasks", len(all))
	}
	got := all[0]
	if got.ID != "coding/plain" || got.Prompt != "Do it." || got.Timeout != DefaultTimeout || got.MaxCostUSDMicro != 2_000_000 || got.Runs != DefaultRuns || got.Bundle || got.Check == nil {
		t.Fatalf("task %+v", got)
	}
}

func TestLoadReadsEveryField(t *testing.T) {
	all, err := Load(filepath.Join("testdata", "suite"))
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].ID != "coding/echo" || all[1].ID != "files/shell" {
		t.Fatalf("tasks %+v", all)
	}
	echo, shell := all[0], all[1]
	if echo.Timeout != 5*time.Minute || echo.MaxCostUSDMicro != 10_000 || echo.Runs != 2 {
		t.Fatalf("echo %+v", echo)
	}
	if shell.Check != nil || shell.Runs != 1 {
		t.Fatalf("shell %+v", shell)
	}
}

func TestLoadTaskRefuses(t *testing.T) {
	task := func(extra string) string {
		return "name: bad\ncategory: coding\nprompt: Do it.\n" + extra
	}
	for _, c := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"no task.yaml", map[string]string{"check.yaml": minimalCheck}, "no such file"},
		{"malformed", map[string]string{"task.yaml": "name: [", "check.yaml": minimalCheck}, "task.yaml"},
		{"unknown field", map[string]string{"task.yaml": task("color: red\n"), "check.yaml": minimalCheck}, "color"},
		{"another name", map[string]string{"task.yaml": "name: other\ncategory: coding\nprompt: x\n", "check.yaml": minimalCheck}, "is not the directory's name"},
		{"another category", map[string]string{"task.yaml": "name: bad\ncategory: files\nprompt: x\n", "check.yaml": minimalCheck}, "is not the parent directory"},
		{"no prompt", map[string]string{"task.yaml": "name: bad\ncategory: coding\nprompt: '  '\n", "check.yaml": minimalCheck}, "no prompt"},
		{"a bad timeout", map[string]string{"task.yaml": task("timeout: soon\n"), "check.yaml": minimalCheck}, "timeout"},
		{"a negative timeout", map[string]string{"task.yaml": task("timeout: -1m\n"), "check.yaml": minimalCheck}, "timeout"},
		{"a zero maxCost", map[string]string{"task.yaml": task("maxCost: 0\n"), "check.yaml": minimalCheck}, "maxCost"},
		{"no runs", map[string]string{"task.yaml": task("runs: 0\n"), "check.yaml": minimalCheck}, "runs"},
		{"an unknown tool", map[string]string{"task.yaml": task("agent: {tools: [read, teleport]}\n"), "check.yaml": minimalCheck}, "teleport"},
		{"a subagent's unknown tool", map[string]string{"task.yaml": task("agent: {subagents: {helper: {tools: [spawn]}}}\n"), "check.yaml": minimalCheck}, "subagent helper"},
		{"serve not a directory", map[string]string{"task.yaml": task("serve: pages\n"), "check.yaml": minimalCheck}, "serve"},
		{"web_search and no results", map[string]string{"task.yaml": task("agent: {tools: [web_search]}\n"), "check.yaml": minimalCheck}, "names no search results"},
		{"results and no web_search", map[string]string{"task.yaml": task("search: r.yaml\n"), "check.yaml": minimalCheck, "r.yaml": "[]\n"}, "does not hold it"},
		{"results that do not read", map[string]string{"task.yaml": task("agent: {tools: [web_search]}\nsearch: r.yaml\n"), "check.yaml": minimalCheck, "r.yaml": "- {title: T, url: ftp://x}\n"}, "http or https URL"},
		{"results with another field", map[string]string{"task.yaml": task("agent: {tools: [web_search]}\nsearch: r.yaml\n"), "check.yaml": minimalCheck, "r.yaml": "- {title: T, url: 'https://x', rank: 1}\n"}, "rank"},
		{"results that are missing", map[string]string{"task.yaml": task("agent: {tools: [web_search]}\nsearch: r.yaml\n"), "check.yaml": minimalCheck}, "search"},
		{"two starts", map[string]string{"task.yaml": task(""), "check.yaml": minimalCheck, "fixture/a.txt": "a", "fixture.bundle": "b"}, "both fixture/"},
		{"no checker", map[string]string{"task.yaml": task("")}, "no checker"},
		{"two checkers", map[string]string{"task.yaml": task(""), "check.yaml": minimalCheck, "check.sh": "exit 0\n"}, "both check.yaml"},
		{"a bad check", map[string]string{"task.yaml": task(""), "check.yaml": "- file: {}\n"}, "no path"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "coding", "bad")
			writeFiles(t, dir, c.files)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			_, err := LoadTask(dir)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want one naming %q", err, c.want)
			}
		})
	}
}

func TestLoadRefusesASuite(t *testing.T) {
	empty := t.TempDir()
	if _, err := Load(empty); err == nil || !strings.Contains(err.Error(), "no task") {
		t.Fatalf("an empty suite: %v", err)
	}
	broken := t.TempDir()
	writeFiles(t, broken, map[string]string{"files/bad/task.yaml": "name: bad\n"})
	if _, err := Load(broken); err == nil {
		t.Fatal("a suite with a broken task loaded")
	}
	notDir := t.TempDir()
	writeFiles(t, notDir, map[string]string{"threads": "a file where a category directory goes"})
	if _, err := Load(notDir); err == nil || strings.Contains(err.Error(), "no task") {
		t.Fatalf("a category that is a file: %v", err)
	}
}

func TestExpandReplacesThePlaceholders(t *testing.T) {
	got := string(expand([]byte("${WORKDIR}/a ${SERVE_URL}/b ${OTHER}"), "/w", "http://h"))
	if got != "/w/a http://h/b ${OTHER}" {
		t.Fatalf("expand = %q", got)
	}
}
