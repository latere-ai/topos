// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package stats

import (
	"errors"
	"testing"
)

func TestMean(t *testing.T) {
	for _, c := range []struct {
		xs   []float64
		want float64
	}{
		{[]float64{1}, 1},
		{[]float64{1, 2, 3, 4}, 2.5},
	} {
		got, err := Mean(c.xs)
		if err != nil || got != c.want {
			t.Errorf("Mean(%v) = %v, %v; want %v", c.xs, got, err, c.want)
		}
	}
	if _, err := Mean(nil); !errors.Is(err, ErrEmpty) {
		t.Errorf("Mean(nil) = %v, want ErrEmpty", err)
	}
}
