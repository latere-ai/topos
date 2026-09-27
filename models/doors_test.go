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
