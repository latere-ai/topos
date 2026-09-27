// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package wordcount

import (
	"maps"
	"testing"
)

func TestCountTabsAndRuns(t *testing.T) {
	got := Count("  one\ttwo   two\r\nthree  ")
	want := map[string]int{"one": 1, "two": 2, "three": 1}
	if !maps.Equal(got, want) {
		t.Fatalf("Count = %v, want %v", got, want)
	}
	if n := len(Count(" \t\n")); n != 0 {
		t.Fatalf("white space alone counted %d words", n)
	}
}
