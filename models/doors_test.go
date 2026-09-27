// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package models

import (
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
)

// TestDoorsPickTheFamilysDoor: a request goes to its family's door, the
// Chat dialect to OpenAI's, never to the translating /lux door; with no
// doors, or none for the family, it goes to the base as configured.
func TestDoorsPickTheFamilysDoor(t *testing.T) {
	root := "https://api.example.com/v1/models"
	ds := Doors{"anthropic": root + "/anthropic/", "openai": root + "/openai", "lux": root + "/lux"}
	for d, want := range map[ir.Dialect]string{
		ir.DialectAnthropicMessages: root + "/anthropic",
		ir.DialectOpenAIResponses:   root + "/openai",
		ir.DialectOpenAIChat:        root + "/openai",
	} {
		if got := ds.Door(root, d); got != want {
			t.Errorf("%s: %q, want %q", d, got, want)
		}
	}
	if got := (Doors{"lux": root + "/lux"}).Door(root, ir.DialectAnthropicMessages); got != root {
		t.Errorf("no anthropic door: %q", got)
	}
	var none Doors
	if got := none.Door("https://api.anthropic.com", ir.DialectAnthropicMessages); got != "https://api.anthropic.com" {
		t.Errorf("no doors: %q", got)
	}
}

func TestNamesADoor(t *testing.T) {
	for base, want := range map[string]bool{
		"https://lux.example/anthropic":             true,
		"https://api.example.com/v1/models/openai/": true,
		"http://127.0.0.1:4000/gemini":              true,
		"https://api.example.com/v1/models":         false,
		"https://api.anthropic.com":                 false,
		"https://openrouter.ai/api":                 false,
	} {
		if got := NamesADoor(base); got != want {
			t.Errorf("NamesADoor(%q) = %v", base, got)
		}
	}
}

// TestDoorsMoveUnderTheRootTheyWereDiscoveredAt: Lux names its doors
// under its public URL; a server that discovered them at another root,
// its in-cluster address, reaches each door under that root, and the
// public root stays readable for whoever must use it. Doors that share
// no root, and no doors, are left as they are.
func TestDoorsMoveUnderTheRootTheyWereDiscoveredAt(t *testing.T) {
	public := "https://api.example.com/v1/models"
	inside := "http://lux.internal:8080/v1/models"
	ds := Doors{"anthropic": public + "/anthropic", "openai": public + "/openai/", "lux": public + "/lux"}
	if got := ds.Root(); got != public {
		t.Errorf("Root = %q, want %q", got, public)
	}
	under := ds.Under(inside + "/")
	for name, want := range map[string]string{"anthropic": inside + "/anthropic", "openai": inside + "/openai", "lux": inside + "/lux"} {
		if under[name] != want {
			t.Errorf("Under: %s = %q, want %q", name, under[name], want)
		}
	}
	if got := under.Door(inside, ir.DialectAnthropicMessages); got != inside+"/anthropic" {
		t.Errorf("the family's door under the root is %q", got)
	}
	if got := ds.Under(public); got["openai"] != public+"/openai" {
		t.Errorf("discovered at the public root, the door is %q", got["openai"])
	}
	for name, mixed := range map[string]Doors{
		"two roots":        {"anthropic": public + "/anthropic", "openai": "https://other.example/openai"},
		"a door not named": {"anthropic": public + "/claude"},
		"a door at a root": {"anthropic": "/anthropic"},
		"no doors":         nil,
	} {
		if got := mixed.Root(); got != "" {
			t.Errorf("%s: Root = %q, want none", name, got)
		}
		if got := mixed.Under(inside); len(got) != len(mixed) || (mixed != nil && got["anthropic"] != mixed["anthropic"]) {
			t.Errorf("%s: Under moved doors that share no root: %v", name, got)
		}
	}
}
