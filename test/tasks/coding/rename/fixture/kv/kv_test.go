// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package kv

import "testing"

func TestFetch(t *testing.T) {
	m := map[string]string{"name": " shop "}
	if v, ok := Fetch(m, "name"); !ok || v != "shop" {
		t.Fatalf("Fetch = %q, %v", v, ok)
	}
	if _, ok := Fetch(m, "port"); ok {
		t.Fatal("Fetch found a missing key")
	}
	if v := FetchOr(m, "port", "8080"); v != "8080" {
		t.Fatalf("FetchOr = %q", v)
	}
}
