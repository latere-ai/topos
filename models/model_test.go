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
