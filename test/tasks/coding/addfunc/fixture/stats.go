// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package stats computes summary statistics of samples.
package stats

import "errors"

// ErrEmpty is a statistic of no values.
var ErrEmpty = errors.New("stats: no values")

// Mean returns the arithmetic mean of xs.
func Mean(xs []float64) (float64, error) {
	if len(xs) == 0 {
		return 0, ErrEmpty
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs)), nil
}
