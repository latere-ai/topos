// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"testing"

	v1 "latere.ai/x/topos/manifest/v1"
)

// TestStoredCountsAreWrittenInOneForm: a trigger's counts are stored as
// compact JSON with the members in the order of their fields.
func TestStoredCountsAreWrittenInOneForm(t *testing.T) {
	a, err := encodeCounts(v1.TriggerCounts{Started: 2})
	if err != nil {
		t.Fatal(err)
	}
	b, err := encodeCounts(v1.TriggerCounts{Started: 2})
	if err != nil || a != b {
		t.Fatalf("two writes of the same counts differ: %q, %q, %v", a, b, err)
	}
	if want := `{"started":2`; len(a) < len(want) || a[:len(want)] != want {
		t.Fatalf("counts are %q", a)
	}
}
