// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session_test

import (
	"errors"
	"strings"
	"testing"

	"latere.ai/x/topos/session"
)

// TestCheckDevice: a device machine names a Cella environment by its
// name and an absolute, clean folder; anything else is refused naming
// what is wrong (spec 080).
func TestCheckDevice(t *testing.T) {
	if err := session.CheckDevice("dev-01jaq7v3k9m2abcd", "/Users/ada/code/site"); err != nil {
		t.Fatalf("a device of a named environment and a clean folder: %v", err)
	}
	for _, c := range []struct{ name, env, dir, want string }{
		{"no environment", "", "/Users/ada", "environment"},
		{"an environment with capitals", "Dev-1", "/Users/ada", "environment"},
		{"an environment past a label", strings.Repeat("a", 64), "/Users/ada", "environment"},
		{"no folder", "dev-1", "", "workdir is required"},
		{"a relative folder", "dev-1", "code/site", "absolute"},
		{"a folder that climbs", "dev-1", "/Users/ada/../bob", "absolute"},
		{"a folder with a trailing slash", "dev-1", "/Users/ada/", "absolute"},
		{"the root", "dev-1", "/", "root directory"},
		{"a control character", "dev-1", "/Users/ada\n/x", "control character"},
		{"a folder past its bound", "dev-1", "/" + strings.Repeat("a", session.MaxDeviceWorkdir), "at most"},
	} {
		err := session.CheckDevice(c.env, c.dir)
		if !errors.Is(err, session.ErrInvalidDevice) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want ErrInvalidDevice naming %q", c.name, err, c.want)
		}
	}
}
