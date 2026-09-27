// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Report is one run of the suite: every run of every task, the summary
// per task, and the suite's pass rate and spend.
type Report struct {
	Model      string    `json:"model"`
	Commit     string    `json:"commit,omitempty"`
	Started    time.Time `json:"started"`
	DurationMS int64     `json:"duration_ms"`
	// Passed and Total count runs; PassRate is Passed over Total.
	Passed        int     `json:"passed"`
	Total         int     `json:"total"`
	PassRate      float64 `json:"pass_rate"`
	SpendUSDMicro int64   `json:"spend_usd_micro"`
	// Incomplete is a suite that stopped before its last run, which
	// never passes a bar.
	Incomplete       bool          `json:"incomplete,omitempty"`
	IncompleteReason string        `json:"incomplete_reason,omitempty"`
	Tasks            []TaskSummary `json:"tasks"`
	Runs             []RunResult   `json:"runs"`
}

// TaskSummary is one task's runs.
type TaskSummary struct {
	Task               string   `json:"task"`
	Runs               int      `json:"runs"`
	Passes             int      `json:"passes"`
	MedianSteps        float64  `json:"median_steps"`
	MedianCostUSDMicro float64  `json:"median_cost_usd_micro"`
	StopReasons        []string `json:"stop_reasons"`
}

func (r *Report) incomplete(reason string) {
	r.Incomplete, r.IncompleteReason = true, reason
}

// finish computes the summary from the runs.
func (r *Report) finish() Report {
	r.Tasks = Summarize(r.Runs)
	r.Passed, r.Total = 0, len(r.Runs)
	for _, x := range r.Runs {
		if x.Passed {
			r.Passed++
		}
	}
	r.PassRate = PassRate(r.Runs)
	return *r
}

// PassRate is the passed runs over all runs; zero runs is a rate of 0.
func PassRate(runs []RunResult) float64 {
	if len(runs) == 0 {
		return 0
	}
	n := 0
	for _, r := range runs {
		if r.Passed {
			n++
		}
	}
	return float64(n) / float64(len(runs))
}

// Summarize groups runs by task, in the order each task first appears:
// its runs, passes, the median steps and cost, and the stop reasons
// seen, sorted.
func Summarize(runs []RunResult) []TaskSummary {
	var order []string
	by := map[string][]RunResult{}
	for _, r := range runs {
		if _, ok := by[r.Task]; !ok {
			order = append(order, r.Task)
		}
		by[r.Task] = append(by[r.Task], r)
	}
	out := make([]TaskSummary, 0, len(order))
	for _, id := range order {
		rs := by[id]
		s := TaskSummary{Task: id, Runs: len(rs), StopReasons: []string{}}
		var steps, cost []float64
		for _, r := range rs {
			if r.Passed {
				s.Passes++
			}
			steps = append(steps, float64(r.Steps))
			cost = append(cost, float64(r.CostUSDMicro))
			stop := string(r.Stop)
			if stop == "" {
				stop = "none"
			}
			if !slices.Contains(s.StopReasons, stop) {
				s.StopReasons = append(s.StopReasons, stop)
			}
		}
		slices.Sort(s.StopReasons)
		s.MedianSteps, s.MedianCostUSDMicro = Median(steps), Median(cost)
		out = append(out, s)
	}
	return out
}

// Median is the middle value, or the mean of the two middle values; an
// empty list is 0.
func Median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Sorted(slices.Values(xs))
	m := len(s) / 2
	if len(s)%2 == 1 {
		return s[m]
	}
	return (s[m-1] + s[m]) / 2
}

// Markdown renders the report as a table per task and a line for the
// suite.
func (r Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Task suite: %s\n\n", r.Model)
	fmt.Fprintf(&b, "Pass rate %.1f%% (%d of %d runs), spend %s", r.PassRate*100, r.Passed, r.Total, usd(r.SpendUSDMicro))
	if r.Commit != "" {
		fmt.Fprintf(&b, ", commit %s", r.Commit)
	}
	b.WriteString(".\n")
	if r.Incomplete {
		fmt.Fprintf(&b, "\nThe run is incomplete: %s.\n", r.IncompleteReason)
	}
	b.WriteString("\n| Task | Runs | Passes | Median steps | Median cost | Stop reasons |\n|---|---|---|---|---|---|\n")
	for _, t := range r.Tasks {
		fmt.Fprintf(&b, "| %s | %d | %d | %g | %s | %s |\n", t.Task, t.Runs, t.Passes, t.MedianSteps, usd(int64(t.MedianCostUSDMicro)), strings.Join(t.StopReasons, ", "))
	}
	var failed []RunResult
	for _, x := range r.Runs {
		if !x.Passed {
			failed = append(failed, x)
		}
	}
	if len(failed) > 0 {
		b.WriteString("\n## Failed runs\n\n")
		for _, x := range failed {
			fmt.Fprintf(&b, "- %s run %d: %s\n", x.Task, x.Run, firstLine(x.Reason))
		}
	}
	return b.String()
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// The files WriteReport writes.
const (
	FileReportJSON     = "report.json"
	FileReportMarkdown = "report.md"
)

// WriteReport writes the report as report.json and report.md in dir.
func WriteReport(dir string, r Report) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("tasks: encode the report: %w", err)
	}
	return errors.Join(
		os.WriteFile(filepath.Join(dir, FileReportJSON), append(b, '\n'), 0o644),
		os.WriteFile(filepath.Join(dir, FileReportMarkdown), []byte(r.Markdown()), 0o644),
	)
}

// ReadReport reads a report.json.
func ReadReport(path string) (Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Report{}, fmt.Errorf("tasks: %w", err)
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		return Report{}, fmt.Errorf("tasks: %s: %w", path, err)
	}
	return r, nil
}
