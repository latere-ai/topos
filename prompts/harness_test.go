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
	if old, err := Execute(CompactionV1, nil); err != nil || old == c || !strings.Contains(old, "Requests:") {
		t.Fatalf("the first compaction prompt %q, %v", old, err)
	}
	if _, err := Execute("compact/compact-v99", nil); err == nil {
		t.Fatal("a compaction prompt that does not exist")
	}
}

// TestTheCompactionPromptKeepsQuestions: the compaction prompt a new
// summary is asked with has a heading for the questions put to a person,
// each with its answer or that it was left to the agent, and says a
// listed question is not asked again, so a summary carries what was
// decided (spec 039). The first version, which a replay of an older
// summary reads, is as it was released.
func TestTheCompactionPromptKeepsQuestions(t *testing.T) {
	c := Text(Compaction)
	for _, want := range []string{"Questions: every question you put to a person with the question tool", "the answer the person gave, quoted exactly", "left to you and what you then assumed", "do not ask it again"} {
		if !strings.Contains(c, want) {
			t.Errorf("the compaction prompt lacks %q:\n%s", want, c)
		}
	}
	if string(Compaction) != "compact/compact-v2" || string(CompactionV1) != "compact/compact-v1" {
		t.Fatalf("the compaction prompts are %s and %s", Compaction, CompactionV1)
	}
	if strings.Contains(Text(CompactionV1), "Questions:") {
		t.Fatal("the released first version changed")
	}
	// The headings a summary was asked for before are all still asked.
	for _, heading := range []string{"Requests:", "Decisions:", "Files:", "Commands:", "Open problems:", "Next step:"} {
		if !strings.Contains(c, heading) || !strings.Contains(Text(CompactionV1), heading) {
			t.Errorf("a compaction prompt lacks the heading %s", heading)
		}
	}
}
