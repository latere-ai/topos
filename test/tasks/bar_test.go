// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func report(t *testing.T, name string) Report {
	t.Helper()
	r, err := ReadReport(filepath.Join("testdata", "reports", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func bar(t *testing.T, name string) Bar {
	t.Helper()
	b, err := LoadBar(filepath.Join("testdata", "bars", name+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestThresholdFromBaselineRuns(t *testing.T) {
	f, err := Measure([]float64{0.90, 0.92, 0.88, 0.91, 0.89})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(f.Baseline-0.90) > 1e-12 || math.Abs(f.Noise-2*math.Sqrt(0.0002)) > 1e-12 || f.Threshold != 0.87 {
		t.Fatalf("figures %+v", f)
	}
	// 0.8 less a noise of 0 is exactly 80%, and floating point must not
	// round it down to 79%.
	if f, err := Measure([]float64{0.8, 0.8, 0.8, 0.8, 0.8}); err != nil || f.Noise != 0 || f.Threshold != 0.8 {
		t.Fatalf("figures %+v, %v", f, err)
	}
	if f, err := Measure([]float64{0, 1, 0, 1, 0}); err != nil || f.Threshold != 0 {
		t.Fatalf("a noise wider than the baseline: %+v, %v", f, err)
	}
	for _, rates := range [][]float64{{0.9, 0.9}, {0.9, 0.9, 0.9, 0.9, 1.2}, {0.9, 0.9, 0.9, 0.9, math.NaN()}} {
		if _, err := Measure(rates); err == nil {
			t.Errorf("Measure(%v) measured", rates)
		}
	}
	b := bar(t, "measured")
	if *b.Baseline != 0.9 || *b.Threshold != 0.87 || len(b.Runs) != BaselineRuns {
		t.Fatalf("bar %+v", b)
	}
}

func TestBarNamesOneModel(t *testing.T) {
	if _, err := LoadBar(filepath.Join("testdata", "bars", "two-models.yaml")); err == nil || !strings.Contains(err.Error(), "models") {
		t.Fatalf("a bar naming two models: %v", err)
	}
	for _, src := range []string{
		"model: [a, b]\n",
		"model: {family: anthropic, connection: https://x}\n",
		"model: {name: m, family: anthropic}\n",
		"model: {name: m, family: google, connection: https://x}\n",
	} {
		if _, err := ParseBar([]byte(src)); err == nil {
			t.Errorf("%q parsed", src)
		}
	}
	b := bar(t, "measured")
	other := report(t, "other-model")
	gating, err := b.Gate(other)
	if gating || err != nil {
		t.Fatalf("another model's 12%% gated the release: %v %v", gating, err)
	}
	unmeasured := bar(t, "unmeasured")
	if gating, err := unmeasured.Gate(other); gating || err != nil {
		t.Fatalf("another model against an unmeasured bar: %v %v", gating, err)
	}
	if gating, err := unmeasured.Gate(report(t, "pinned-above")); !gating || !errors.Is(err, ErrUnmeasured) {
		t.Fatalf("the pinned model against an unmeasured bar: %v %v", gating, err)
	}
}

func TestReleaseBarFailsBelowThreshold(t *testing.T) {
	b := bar(t, "measured")
	for _, c := range []struct {
		report string
		pass   bool
	}{
		{"pinned-below", false},
		{"pinned-at", true},
		{"pinned-above", true},
		{"pinned-incomplete", false},
	} {
		gating, err := b.Gate(report(t, c.report))
		if !gating || (err == nil) != c.pass {
			t.Errorf("%s: gating %v, err %v, want pass %v", c.report, gating, err, c.pass)
		}
	}
}

func TestParseBarChecksItsFigures(t *testing.T) {
	const head = "model: {name: m, family: other, connection: http://localhost:1}\n"
	runs := "runs:\n" + strings.Repeat("  - {ref: r, pass_rate: 0.9}\n", 5)
	for _, c := range []struct{ src, want string }{
		{head + "baseline: 0.9\n", "sets 1 of"},
		{head + "baseline: 0.9\nnoise: 0\nthreshold: 0.9\nruns:\n  - {ref: r, pass_rate: 0.9}\n", "from 5 runs"},
		{head + "baseline: 0.9\nnoise: 0\nthreshold: 0.9\nruns:\n" + strings.Repeat("  - {pass_rate: 0.9}\n", 5), "has no ref"},
		{head + "baseline: 0.8\nnoise: 0\nthreshold: 0.8\n" + runs, "is not the mean"},
		{head + "baseline: 0.9\nnoise: 0.1\nthreshold: 0.8\n" + runs, "is not twice"},
		{head + "baseline: 0.9\nnoise: 0\nthreshold: 0.85\n" + runs, "rounded down"},
		{head + "colour: blue\n", "colour"},
	} {
		_, err := ParseBar([]byte(c.src))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err %v, want one naming %q", c.src, err, c.want)
		}
	}
	if _, err := ParseBar([]byte(head + "baseline: 0.9\nnoise: 0\nthreshold: 0.9\n" + runs)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBar(filepath.Join(t.TempDir(), FileBar)); err == nil {
		t.Fatal("a missing bar loaded")
	}
}

func TestIRAgainstSDKComparison(t *testing.T) {
	b := bar(t, "measured")
	irRun := report(t, "pinned-above")
	if err := Agree(irRun, report(t, "sdk-close"), *b.Noise); err != nil {
		t.Fatalf("a two point gap within the noise: %v", err)
	}
	if err := Agree(irRun, report(t, "sdk-far"), *b.Noise); err == nil || !strings.Contains(err.Error(), "past the noise") {
		t.Fatalf("a thirteen point gap: %v", err)
	}
	if err := Agree(report(t, "pinned-incomplete"), irRun, *b.Noise); err == nil {
		t.Fatal("an incomplete run was compared")
	}
}
