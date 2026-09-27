// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/session"
)

func TestSummarizeGroupsRunsByTask(t *testing.T) {
	runs := []RunResult{
		{Task: "coding/a", Passed: true, Steps: 10, CostUSDMicro: 300, Stop: session.StopCompleted},
		{Task: "coding/b", Passed: false, Steps: 4, CostUSDMicro: 100, Stop: session.StopBudget},
		{Task: "coding/a", Passed: false, Steps: 30, CostUSDMicro: 900, Stop: session.StopTurnLimit},
		{Task: "coding/a", Passed: true, Steps: 20, CostUSDMicro: 600, Stop: session.StopCompleted},
		{Task: "coding/b", Passed: false},
	}
	got := Summarize(runs)
	want := []TaskSummary{
		{Task: "coding/a", Runs: 3, Passes: 2, MedianSteps: 20, MedianCostUSDMicro: 600, StopReasons: []string{"completed", "turn_limit"}},
		{Task: "coding/b", Runs: 2, Passes: 0, MedianSteps: 2, MedianCostUSDMicro: 50, StopReasons: []string{"budget", "none"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Summarize =\n%+v\nwant\n%+v", got, want)
	}
	if r := PassRate(runs); r != 0.4 {
		t.Fatalf("PassRate = %v", r)
	}
	if PassRate(nil) != 0 || Median(nil) != 0 || len(Summarize(nil)) != 0 {
		t.Fatal("no runs")
	}
}

func TestTheReportIsWrittenAndRead(t *testing.T) {
	start := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	rep := Report{Model: "claude-sonnet-4-5", Commit: "abc123", Started: start, Runs: []RunResult{
		{Task: "coding/a", Run: 1, Passed: true, Steps: 3, CostUSDMicro: 1500, Stop: session.StopCompleted},
		{Task: "coding/a", Run: 2, Passed: false, Reason: "the checker failed at file assertion 1: a.txt does not exist\nmore detail", Stop: session.StopCompleted},
	}}
	rep.SpendUSDMicro = 1500
	rep.incomplete("the suite's budget of $1.0000 is spent")
	rep = rep.finish(time.Now())
	dir := t.TempDir()
	if err := WriteReport(dir, rep); err != nil {
		t.Fatal(err)
	}
	back, err := ReadReport(filepath.Join(dir, FileReportJSON))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, rep) {
		t.Fatalf("read back\n%+v\nwrote\n%+v", back, rep)
	}
	md, err := os.ReadFile(filepath.Join(dir, FileReportMarkdown))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# Task suite: claude-sonnet-4-5",
		"Pass rate 50.0% (1 of 2 runs), spend $0.0015, commit abc123.",
		"The run is incomplete: the suite's budget of $1.0000 is spent.",
		"| coding/a | 2 | 1 | 1.5 | $0.0008 | completed |",
		"- coding/a run 2: the checker failed at file assertion 1: a.txt does not exist\n",
	} {
		if !strings.Contains(string(md), want) {
			t.Errorf("the Markdown lacks %q:\n%s", want, md)
		}
	}
	empty := Report{Model: "m"}
	plain := empty.finish(time.Now()).Markdown()
	if strings.Contains(plain, "commit") || strings.Contains(plain, "incomplete") || strings.Contains(plain, "Failed runs") {
		t.Fatalf("an empty report says more than it holds:\n%s", plain)
	}
}

func TestReportFilesThatCannotBeUsed(t *testing.T) {
	if err := WriteReport(filepath.Join(t.TempDir(), "missing"), Report{}); err == nil {
		t.Fatal("a report was written into a directory that does not exist")
	}
	if _, err := ReadReport(filepath.Join(t.TempDir(), "none.json")); err == nil {
		t.Fatal("a missing report was read")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadReport(bad); err == nil {
		t.Fatal("a report that is not JSON was read")
	}
}
