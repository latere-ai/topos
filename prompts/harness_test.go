// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package prompts

import (
	"strings"
	"testing"
)

func TestRenderIncludesOnlyTheSectionsThatApply(t *testing.T) {
	plain, err := Harness(HarnessCurrent, HarnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, "{{") || !strings.Contains(plain, "disposable sandbox") || strings.Contains(plain, "# Threads") || strings.Contains(plain, "# Git") {
		t.Fatalf("a plain sandbox prompt:\n%s", plain)
	}
	full, err := Harness(HarnessCurrent, HarnessOptions{Host: true, Threads: true, Memory: true, Git: true})
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
	if _, err := Harness(99, HarnessOptions{}); err == nil {
		t.Fatal("rendered a version that does not exist")
	}
	if HarnessVersion(HarnessCurrent) != "harness/1" {
		t.Fatalf("version %s", HarnessVersion(HarnessCurrent))
	}
}

func TestCompact(t *testing.T) {
	c, err := Execute(Compaction, nil)
	if err != nil || !strings.Contains(c, "Requests:") || !strings.Contains(c, "Do not call any tool") {
		t.Fatalf("compact prompt %q, %v", c, err)
	}
	if _, err := Execute("compact/compact-v99", nil); err == nil {
		t.Fatal("a compaction prompt that does not exist")
	}
}
