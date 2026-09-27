// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/goccy/go-yaml"

	"latere.ai/x/topos/models"
)

// FileBar is the release bar's file in the suite's root.
const FileBar = "bar.yaml"

// BaselineRuns is how many full runs of the suite measure a baseline.
const BaselineRuns = 5

// epsilon absorbs the rounding of rates written as decimals.
const epsilon = 1e-9

// Bar is bar.yaml: the one model that gates a release, and the figures
// measured on it from the recorded runs. A bar whose figures are absent
// names its model before the baseline is measured.
type Bar struct {
	Model     BarModel `yaml:"model" json:"model"`
	Baseline  *float64 `yaml:"baseline" json:"baseline,omitempty"`
	Noise     *float64 `yaml:"noise" json:"noise,omitempty"`
	Threshold *float64 `yaml:"threshold" json:"threshold,omitempty"`
	Runs      []BarRun `yaml:"runs" json:"runs,omitempty"`
}

// BarModel is the pinned model: its catalog name, its family, and the
// connection its release runs use.
type BarModel struct {
	Name       string `yaml:"name" json:"name"`
	Family     string `yaml:"family" json:"family"`
	Connection string `yaml:"connection" json:"connection"`
}

// BarRun is one recorded release run the figures come from.
type BarRun struct {
	Ref      string  `yaml:"ref" json:"ref"`
	PassRate float64 `yaml:"pass_rate" json:"pass_rate"`
}

// Figures are a bar's measured numbers, as fractions of 1.
type Figures struct {
	Baseline  float64 `json:"baseline"`
	Noise     float64 `json:"noise"`
	Threshold float64 `json:"threshold"`
}

// Measure computes the figures from the pass rates of the baseline
// runs: the baseline is their mean, the noise twice their population
// standard deviation, and the threshold the baseline less the noise,
// rounded down to a whole percent and never below zero.
func Measure(rates []float64) (Figures, error) {
	if len(rates) != BaselineRuns {
		return Figures{}, fmt.Errorf("tasks: a baseline is measured from %d runs, not %d", BaselineRuns, len(rates))
	}
	var sum float64
	for _, r := range rates {
		if r < 0 || r > 1 || math.IsNaN(r) {
			return Figures{}, fmt.Errorf("tasks: pass rate %v is not between 0 and 1", r)
		}
		sum += r
	}
	mean := sum / float64(len(rates))
	var sq float64
	for _, r := range rates {
		sq += (r - mean) * (r - mean)
	}
	noise := 2 * math.Sqrt(sq/float64(len(rates)))
	threshold := max(0, math.Floor((mean-noise)*100+epsilon)/100)
	return Figures{Baseline: mean, Noise: noise, Threshold: threshold}, nil
}

// LoadBar reads and checks a bar.yaml.
func LoadBar(path string) (Bar, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Bar{}, fmt.Errorf("tasks: %w", err)
	}
	return ParseBar(b)
}

// ParseBar reads a bar and checks it: exactly one model with its name,
// family and connection, and either no figures or all of them, from
// BaselineRuns recorded runs whose rates give those figures.
func ParseBar(b []byte) (Bar, error) {
	var bar Bar
	if err := yaml.UnmarshalWithOptions(b, &bar, yaml.DisallowUnknownField()); err != nil {
		return Bar{}, fmt.Errorf("tasks: %s: %w", FileBar, err)
	}
	m := bar.Model
	switch {
	case strings.TrimSpace(m.Name) == "" || m.Connection == "":
		return Bar{}, fmt.Errorf("tasks: %s names no gating model: it needs a model with a name and a connection", FileBar)
	case m.Family != models.FamilyAnthropic && m.Family != models.FamilyOpenAI && m.Family != models.FamilyOther:
		return Bar{}, fmt.Errorf("tasks: %s: family %q is not anthropic, openai or other", FileBar, m.Family)
	}
	set := 0
	for _, f := range []*float64{bar.Baseline, bar.Noise, bar.Threshold} {
		if f != nil {
			set++
		}
	}
	if set == 0 && len(bar.Runs) == 0 {
		return bar, nil
	}
	if set != 3 {
		return Bar{}, fmt.Errorf("tasks: %s sets %d of baseline, noise and threshold; a measured bar sets all three", FileBar, set)
	}
	rates := make([]float64, len(bar.Runs))
	for i, r := range bar.Runs {
		if r.Ref == "" {
			return Bar{}, fmt.Errorf("tasks: %s: run %d has no ref", FileBar, i+1)
		}
		rates[i] = r.PassRate
	}
	f, err := Measure(rates)
	if err != nil {
		return Bar{}, err
	}
	const decimals = 1e-4
	switch {
	case math.Abs(f.Baseline-*bar.Baseline) > decimals:
		return Bar{}, fmt.Errorf("tasks: %s: baseline %v is not the mean %.4f of its runs", FileBar, *bar.Baseline, f.Baseline)
	case math.Abs(f.Noise-*bar.Noise) > decimals:
		return Bar{}, fmt.Errorf("tasks: %s: noise %v is not twice the standard deviation %.4f of its runs", FileBar, *bar.Noise, f.Noise)
	case math.Abs(f.Threshold-*bar.Threshold) > epsilon:
		return Bar{}, fmt.Errorf("tasks: %s: threshold %v is not the baseline less the noise rounded down, %.2f", FileBar, *bar.Threshold, f.Threshold)
	}
	return bar, nil
}

// ErrUnmeasured is a gating report against a bar with no threshold yet.
var ErrUnmeasured = errors.New("tasks: the bar has no measured threshold")

// Gate judges a report against the bar. A report of any other model is
// not gating and never fails a release. A report of the pinned model
// fails when the bar is unmeasured, when the run is incomplete, and
// when its pass rate is below the threshold; at or above it passes.
func (b Bar) Gate(r Report) (gating bool, err error) {
	if r.Model != b.Model.Name {
		return false, nil
	}
	switch {
	case b.Threshold == nil:
		return true, ErrUnmeasured
	case r.Incomplete:
		return true, fmt.Errorf("tasks: the run of %s is incomplete (%s), which never passes the bar", r.Model, r.IncompleteReason)
	case r.PassRate+epsilon < *b.Threshold:
		return true, fmt.Errorf("tasks: %s passed %.1f%% of runs, below the threshold of %.0f%%", r.Model, r.PassRate*100, *b.Threshold*100)
	}
	return true, nil
}

// Agree compares a suite run through the IR path with one through the
// provider's own SDK: their pass rates must be within the noise, and
// neither may be incomplete.
func Agree(irRun, sdkRun Report, noise float64) error {
	if irRun.Incomplete || sdkRun.Incomplete {
		return errors.New("tasks: an incomplete run cannot be compared")
	}
	if gap := math.Abs(irRun.PassRate - sdkRun.PassRate); gap > noise+epsilon {
		return fmt.Errorf("tasks: the IR path passed %.1f%% and the SDK path %.1f%%, a gap of %.1f points past the noise of %.1f",
			irRun.PassRate*100, sdkRun.PassRate*100, gap*100, noise*100)
	}
	return nil
}
