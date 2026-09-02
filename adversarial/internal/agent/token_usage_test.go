// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package agent

import "testing"

func TestTokenUsage_Total(t *testing.T) {
	u := TokenUsage{Input: 1, Output: 2, CacheCreate: 3, CacheRead: 4}
	if got := u.Total(); got != 10 {
		t.Errorf("Total: got %d, want 10", got)
	}
}

func TestTokenUsage_Add(t *testing.T) {
	u := TokenUsage{Input: 1, Output: 2, CacheCreate: 3, CacheRead: 4}
	u.Add(TokenUsage{Input: 10, Output: 20, CacheCreate: 30, CacheRead: 40})
	if u.Input != 11 || u.Output != 22 || u.CacheCreate != 33 || u.CacheRead != 44 {
		t.Errorf("Add: %+v", u)
	}
}
