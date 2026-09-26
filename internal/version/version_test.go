// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package version

import "testing"

func TestStringNamesTheBinaryAndTheBuild(t *testing.T) {
	old := [3]string{Version, Commit, Date}
	t.Cleanup(func() { Version, Commit, Date = old[0], old[1], old[2] })

	Version, Commit, Date = "v0.8.0", "abc1234", "2026-09-27T00:00:00Z"
	if got, want := String("toposd"), "toposd v0.8.0 (abc1234, 2026-09-27T00:00:00Z)"; got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}
}

func TestDefaultsMarkADevelopmentBuild(t *testing.T) {
	if Version != "dev" || Commit != "none" || Date != "unknown" {
		t.Fatalf("defaults = %q %q %q, want dev none unknown", Version, Commit, Date)
	}
}
