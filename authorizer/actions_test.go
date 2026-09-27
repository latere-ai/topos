// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer

import (
	"errors"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestVocabularyMatchesTheSpec holds the table in spec 006 and the
// package to one set: every action the spec lists is a constant here
// acting on the kind the spec names, and nothing here is missing from
// the spec.
func TestVocabularyMatchesTheSpec(t *testing.T) {
	// A spec that is complete moves to specs/.archive keeping its name.
	raw, err := os.ReadFile("../specs/006-identity.md")
	if errors.Is(err, fs.ErrNotExist) {
		raw, err = os.ReadFile("../specs/.archive/006-identity.md")
	}
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(raw), "### The action vocabulary")
	if !ok {
		t.Fatal("spec 006 has no action vocabulary section")
	}
	section, _, _ = strings.Cut(section, "\n### ")
	tick := regexp.MustCompile("`([a-z_.]+)`")
	spec := map[string]string{}
	var order []string
	for line := range strings.SplitSeq(section, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 4 || !strings.HasPrefix(strings.TrimSpace(cells[1]), "`") {
			continue
		}
		kind := tick.FindStringSubmatch(cells[2])
		if kind == nil {
			t.Fatalf("row %q names no kind", line)
		}
		for _, m := range tick.FindAllStringSubmatch(cells[1], -1) {
			spec[m[1]] = kind[1]
			order = append(order, m[1])
		}
	}
	if !slices.Equal(order, Actions()) {
		t.Fatalf("the spec lists\n%v\nand the package\n%v", order, Actions())
	}
	for _, a := range Vocabulary().Actions {
		if spec[a.Name] != a.Kind {
			t.Errorf("%s acts on %q here and %q in the spec", a.Name, a.Kind, spec[a.Name])
		}
	}
}

func TestVocabularyLookups(t *testing.T) {
	v := Vocabulary()
	if v.Core != Core || len(v.Actions) != len(table) {
		t.Fatalf("Vocabulary = %+v", v)
	}
	v.Actions[0].Name = "changed"
	if Vocabulary().Actions[0].Name != ActionAgentCreate {
		t.Fatal("a caller's edit reached the table")
	}
	for _, kind := range []string{KindAgent, KindSession, KindTrigger, KindCredential, KindMemoryStore} {
		if Vocabulary().Label(kind) == "" {
			t.Errorf("%s has no heading", kind)
		}
		if Kind(Create(kind)) != kind || !strings.HasSuffix(Create(kind), ".create") {
			t.Errorf("Create(%s) = %q", kind, Create(kind))
		}
	}
	if !Known(ActionSessionScope) || Known("session.explode") || Kind("session.explode") != "" || Create("sandbox") != "" {
		t.Fatal("lookups outside the table")
	}
}
