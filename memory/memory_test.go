// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"strings"
	"testing"
)

// TestUnderAndLocate: a path is memory only absolute and under /memory
// once cleaned, and is located by its folder and its path in it.
func TestUnderAndLocate(t *testing.T) {
	for p, want := range map[string]bool{
		"/memory": true, "/memory/": true, "/memory/user/a.md": true, "/memory/user/../project/a.md": true,
		"/memory/../etc/passwd": false, "/memoryx/a": false, "memory/user/a.md": false, "/workspace/memory/a": false, "": false,
	} {
		if Under(p) != want {
			t.Errorf("Under(%q) = %t", p, !want)
		}
	}
	for p, want := range map[string][2]string{"/memory": {"", ""}, "/memory/user": {"user", ""}, "/memory/user/notes/a.md": {"user", "notes/a.md"}} {
		if name, rel, ok := Locate(p); !ok || name != want[0] || rel != want[1] {
			t.Errorf("Locate(%q) = %q %q %t", p, name, rel, ok)
		}
	}
	if _, _, ok := Locate("/workspace/a"); ok {
		t.Error("located a path outside /memory")
	}
}

// TestTheErrorsSayWhatHappened: each error names what it refused, and a
// refusal is named by the authorizer's reason when it gave one.
func TestTheErrorsSayWhatHappened(t *testing.T) {
	for err, want := range map[error]string{
		&Unreachable{Detail: "the store answered 503"}:                        "could not be reached: the store answered 503",
		&ReadOnly{Folder: "user"}:                                             "/memory/user/ is read only",
		&TooLarge{Path: "/memory/a.md", Size: 2 << 20}:                        "/memory/a.md is 2097152 bytes",
		&Refusal{Op: "write", Status: 403, Code: "forbidden", Message: "No."}: "refused the write with 403 forbidden: No.",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q does not say %q", err.Error(), want)
		}
	}
	if n := (&Refusal{Code: "forbidden", Reason: "memory_off"}).Name(); n != "memory_off" {
		t.Errorf("a refusal with a reason is named %q", n)
	}
	if n := (&Refusal{Code: "forbidden"}).Name(); n != "forbidden" {
		t.Errorf("a refusal with no reason is named %q", n)
	}
	if u := (&Unreachable{Detail: "d"}); u.Unwrap() != nil {
		t.Error("an unreachable store with no cause unwraps to one")
	}
}
