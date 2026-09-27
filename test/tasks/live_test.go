// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build tasks || instructions

package tasks

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestTheSuiteAgainstAModel is the tasks tier of spec 026, and with the
// instructions tag its instruction tier: the suite run against the real
// model the environment names, its report written as report.json and
// report.md beside the runs it keeps. It fails when the run is
// incomplete, and when the model is the one bar.yaml pins and its pass
// rate is below the threshold. Without a model connection it skips.
func TestTheSuiteAgainstAModel(t *testing.T) {
	o, err := FromEnv(os.Getenv)
	if errors.Is(err, ErrNoModel) {
		t.Skip(err.Error())
	}
	if err != nil {
		t.Fatal(err)
	}
	if o.Filter == "" {
		o.Filter = tierFilter
	}
	out := os.Getenv(EnvOut)
	if out == "" {
		if out, err = os.MkdirTemp("", "topos-tasks-"); err != nil {
			t.Fatal(err)
		}
	}
	o.Dir, o.Work = ".", filepath.Join(out, "runs")
	if err := os.MkdirAll(o.Work, 0o755); err != nil {
		t.Fatal(err)
	}
	if deadline, ok := t.Deadline(); ok {
		t.Logf("go test ends this run at %s; a whole suite needs -timeout 0", deadline.Format(time.RFC3339))
	}
	// The report so far is written after every run, so a run cut short
	// by the test's timeout still leaves one.
	partial := Report{Model: o.Connection.Model, Commit: o.Commit}
	start := time.Now()
	o.OnRun = func(r RunResult) {
		t.Logf("%s run %d: passed %v, %d steps, %s, %s", r.Task, r.Run, r.Passed, r.Steps, usd(r.CostUSDMicro), r.Reason)
		partial.Runs = append(partial.Runs, r)
		partial.SpendUSDMicro += r.CostUSDMicro
		p := partial
		p.incomplete("the run was still going when this report was written")
		if err := WriteReport(out, p.finish(start)); err != nil {
			t.Errorf("write the report so far: %v", err)
		}
	}
	rep, err := Run(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteReport(out, rep); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s passed %d of %d runs (%.1f%%) for %s; the report and the runs are in %s", rep.Model, rep.Passed, rep.Total, rep.PassRate*100, usd(rep.SpendUSDMicro), out)
	if rep.Incomplete {
		t.Fatalf("the run is incomplete: %s", rep.IncompleteReason)
	}
	b, err := LoadBar(FileBar)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if gating, err := b.Gate(rep); err != nil {
		t.Fatal(err)
	} else if gating {
		t.Logf("%s is at or above the bar's threshold of %.0f%%", rep.Model, *b.Threshold*100)
	}
}
