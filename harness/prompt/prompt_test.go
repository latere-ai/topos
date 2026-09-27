// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package prompt

import (
	"strings"
	"testing"
)

func TestRenderIncludesOnlyTheSectionsThatApply(t *testing.T) {
	plain, err := Render(Current, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, "{{") || !strings.Contains(plain, "disposable sandbox") || strings.Contains(plain, "# Threads") || strings.Contains(plain, "# Git") {
		t.Fatalf("a plain sandbox prompt:\n%s", plain)
	}
	full, err := Render(Current, Options{Host: true, Threads: true, Memory: true, Git: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"person's own computer", "# Threads", "`spawn`", "# Memory", "# Git", "unknown_effect", "is an answer, not an obstacle"} {
		if !strings.Contains(full, want) {
			t.Fatalf("the full prompt lacks %q", want)
		}
	}
	if strings.HasSuffix(full, "\n") {
		t.Fatal("the prompt ends with a newline")
	}
	if _, err := Render(99, Options{}); err == nil {
		t.Fatal("rendered a version that does not exist")
	}
	if Version(Current) != "harness/1" {
		t.Fatalf("version %s", Version(Current))
	}
}

func TestCompact(t *testing.T) {
	c, err := Compact(CompactCurrent)
	if err != nil || !strings.Contains(c, "Requests:") || !strings.Contains(c, "Do not call any tool") {
		t.Fatalf("compact prompt %q, %v", c, err)
	}
	if _, err := Compact(99); err == nil {
		t.Fatal("a compaction prompt that does not exist")
	}
}
