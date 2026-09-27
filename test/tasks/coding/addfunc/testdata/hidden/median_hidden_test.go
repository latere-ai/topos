// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package stats

import (
	"errors"
	"slices"
	"testing"
)

func TestMedianHidden(t *testing.T) {
	for _, c := range []struct {
		xs   []float64
		want float64
	}{
		{[]float64{7}, 7},
		{[]float64{3, 1, 2}, 2},
		{[]float64{4, 1, 3, 2}, 2.5},
		{[]float64{-5, 10}, 2.5},
		{[]float64{2, 2, 2, 9}, 2},
	} {
		in := slices.Clone(c.xs)
		got, err := Median(in)
		if err != nil || got != c.want {
			t.Errorf("Median(%v) = %v, %v; want %v", c.xs, got, err, c.want)
		}
		if !slices.Equal(in, c.xs) {
			t.Errorf("Median reordered its input to %v", in)
		}
	}
	if _, err := Median(nil); !errors.Is(err, ErrEmpty) {
		t.Errorf("Median(nil) = %v, want ErrEmpty", err)
	}
	if _, err := Median([]float64{}); !errors.Is(err, ErrEmpty) {
		t.Errorf("Median of an empty slice = %v, want ErrEmpty", err)
	}
}
