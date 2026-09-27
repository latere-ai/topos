// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package kv

import "testing"

func TestLookupHidden(t *testing.T) {
	if v, ok := Lookup(map[string]string{"k": " v "}, "k"); !ok || v != "v" {
		t.Fatalf("Lookup = %q, %v", v, ok)
	}
	if FetchOr(map[string]string{}, "k", "d") != "d" {
		t.Fatal("FetchOr is a different function and keeps its name")
	}
}
