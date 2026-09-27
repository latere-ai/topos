// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package report

import "testing"

func TestSummary(t *testing.T) {
	got := Summary(map[string]string{"name": "Corner", "port": "9000", "owner": "Ada"})
	if got != "Corner on port 9000" {
		t.Fatalf("Summary = %q", got)
	}
	if Owner(map[string]string{"owner": "Ada"}) != "Ada" {
		t.Fatal("Owner")
	}
}
