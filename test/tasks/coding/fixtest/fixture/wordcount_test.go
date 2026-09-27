// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package wordcount

import (
	"maps"
	"testing"
)

func TestCount(t *testing.T) {
	got := Count("The cat sat.\nThe dog sat, too!")
	want := map[string]int{"the": 2, "cat": 1, "sat": 2, "dog": 1, "too": 1}
	if !maps.Equal(got, want) {
		t.Fatalf("Count = %v, want %v", got, want)
	}
}
