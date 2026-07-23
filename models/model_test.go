// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package models_test

import (
	"testing"

	"latere.ai/x/topos/models"
)

// TestUsageAdd asserts Add folds every field of one Usage into another, which is
// how the loop totals per-turn usage and how cost consumers fold session usage.
func TestUsageAdd(t *testing.T) {
	total := models.Usage{InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 4}
	total.Add(models.Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CacheWriteTokens: 40})

	want := models.Usage{InputTokens: 11, OutputTokens: 22, CacheReadTokens: 33, CacheWriteTokens: 44}
	if total != want {
		t.Fatalf("Add result = %+v, want %+v", total, want)
	}
}

// TestUsageAddZeroIsIdentity asserts adding a zero Usage changes nothing.
func TestUsageAddZeroIsIdentity(t *testing.T) {
	u := models.Usage{InputTokens: 7, OutputTokens: 8}
	u.Add(models.Usage{})
	if (u != models.Usage{InputTokens: 7, OutputTokens: 8}) {
		t.Fatalf("adding zero changed the value: %+v", u)
	}
}

// TestUsageAddCostNilVersusZero asserts a nil CostUSDMicro and a reported zero
// take different paths through Add: zero is a real cost that sums, nil is
// unknown and poisons the total so the caller prices it from a rate card
// instead of under-counting it.
func TestUsageAddCostNilVersusZero(t *testing.T) {
	zero, five := int64(0), int64(5)

	reported := models.Usage{InputTokens: 1, CostUSDMicro: &five}
	reported.Add(models.Usage{InputTokens: 1, CostUSDMicro: &zero})
	if reported.CostUSDMicro == nil {
		t.Fatal("reported + reported-zero = nil cost, want a summed cost")
	}
	if got := *reported.CostUSDMicro; got != 5 {
		t.Fatalf("reported + reported-zero cost = %d, want 5", got)
	}

	unreported := models.Usage{InputTokens: 1, CostUSDMicro: &five}
	unreported.Add(models.Usage{InputTokens: 1})
	if unreported.CostUSDMicro != nil {
		t.Fatalf("reported + unreported cost = %d, want nil (unknown)", *unreported.CostUSDMicro)
	}

	// The reverse order is equally unknown: a nil accumulator that is not the
	// zero Usage cannot absorb a reported cost.
	partial := models.Usage{InputTokens: 1}
	partial.Add(models.Usage{InputTokens: 1, CostUSDMicro: &five})
	if partial.CostUSDMicro != nil {
		t.Fatalf("unreported + reported cost = %d, want nil (unknown)", *partial.CostUSDMicro)
	}
}

// TestUsageAddCostFromZeroValue asserts the zero Usage is the additive identity
// for cost too: the accumulator's starting state must not be read as a turn
// whose cost went unreported, or a run's first reported cost would be lost.
func TestUsageAddCostFromZeroValue(t *testing.T) {
	seven := int64(7)
	var total models.Usage
	total.Add(models.Usage{InputTokens: 1, CostUSDMicro: &seven})
	if total.CostUSDMicro == nil {
		t.Fatal("zero-value accumulator dropped a reported cost")
	}
	if got := *total.CostUSDMicro; got != 7 {
		t.Fatalf("cost = %d, want 7", got)
	}

	// The accumulator owns its own pointee, so mutating the source afterwards
	// must not change the total.
	seven = 99
	if got := *total.CostUSDMicro; got != 7 {
		t.Fatalf("cost aliased the source: %d, want 7", got)
	}
}
