// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/topos/models"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const openRouter = `{"data":[
 {"id":"anthropic/claude-opus-5","context_length":1000000,"top_provider":{"max_completion_tokens":128000},"architecture":{"input_modalities":["text","image"]},"supported_parameters":["tools","reasoning"]},
 {"id":"google/flash-preview","context_length":1048576,"top_provider":{"max_completion_tokens":65536},"architecture":{"input_modalities":["text","image","audio"]},"supported_parameters":["tools"]},
 {"id":"anthropic/claude-opus-5:batch","context_length":1,"top_provider":{"max_completion_tokens":1}},
 {"id":"openai/gpt-5.6","context_length":400000,"top_provider":{"max_completion_tokens":128000},"supported_parameters":["tools","parallel_tool_calls"]},
 {"id":"vendor/small:free","context_length":65536,"top_provider":{"max_completion_tokens":8192},"architecture":{"input_modalities":["text","image"]},"supported_parameters":["tools"]},
 {"id":"vendor/safety:free","context_length":65536,"top_provider":{"max_completion_tokens":8192},"supported_parameters":[]}
]}`

func model(name, provider, upstream, pricing string) string {
	return "kind: Model\nmetadata:\n  name: " + name + "\nspec:\n  targets:\n    - provider: " + provider + "\n      model: " + upstream + "\n" + pricing
}

func TestRunMergesPricesAndWindows(t *testing.T) {
	dir := t.TempDir()
	lux := filepath.Join(dir, "models")
	if err := os.Mkdir(lux, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(lux, "anthropic__claude-opus-5.yaml"), model("anthropic/claude-opus-5", "anthropic", "claude-opus-5",
		"  pricing:\n    per: 1000000\n    input: \"5\"\n    output: \"25\"\n    cachedInput: \"0.5\"\n  modalities:\n    input: [text, image]\n"))
	write(t, filepath.Join(lux, "openai__gpt-5-6.yaml"), model("openai/gpt-5-6", "openai", "gpt-5.6", "  modalities:\n    input: [text, image]\n"))
	write(t, filepath.Join(lux, "gemini__flash.yaml"), model("gemini/flash", "gemini", "flash", ""))
	write(t, filepath.Join(lux, "zhipu__glm.yaml"), model("zhipu/glm", "zhipu", "glm", ""))
	or := filepath.Join(dir, "openrouter.json")
	write(t, or, openRouter)
	out := filepath.Join(dir, "catalog.json")
	if err := run(lux, "", "", or, "2026-09-27", out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var c models.Catalog
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.Source, "2026-09-27") || len(c.Models) != 5 {
		t.Fatalf("catalog %+v", c)
	}
	opus, ok := c.Lookup("claude-opus-5")
	if !ok || opus.Family != models.FamilyAnthropic || opus.InputWindow != 1_000_000 || opus.MaxOutputTokens != 128_000 || *opus.Pricing.CacheRead != 500_000 || !opus.Supports.Images || !opus.Supports.Thinking {
		t.Fatalf("opus %+v", opus)
	}
	gpt, ok := c.Lookup("gpt-5.6")
	if !ok || gpt.Dialect != "openai-responses" || gpt.InputWindow != 400_000 || !gpt.Supports.ParallelTools || gpt.Pricing != nil {
		t.Fatalf("gpt %+v", gpt)
	}
	// The hosted Lux's spelling, dots in the version, names the same entry.
	if dotted, ok := c.Lookup("openai/gpt-5.6"); !ok || dotted.Name != "openai/gpt-5-6" {
		t.Fatalf("openai/gpt-5.6 = %+v, %v", dotted, ok)
	}
	if opus.Aliases[len(opus.Aliases)-1] == "anthropic/claude-opus-5" {
		t.Fatalf("an alias repeats the name: %v", opus.Aliases)
	}
	// A Gemini model is spoken to through Lux's OpenAI door, and OpenRouter
	// names it under google, in preview.
	flash, ok := c.Lookup("gemini/flash")
	if !ok || flash.Family != models.FamilyOther || flash.Dialect != "openai-chat" || flash.InputWindow != 1_048_576 || !flash.Supports.Images {
		t.Fatalf("flash %+v", flash)
	}
	// Whether a model takes images is OpenRouter's alone: a Lux manifest
	// that declares images for a model OpenRouter lists without them does
	// not make it take them.
	if gpt.Supports.Images {
		t.Fatalf("gpt takes images though OpenRouter lists none: %+v", gpt)
	}
	glm, ok := c.Lookup("zhipu/glm")
	if !ok || glm.Dialect != "openai-chat" || glm.InputWindow != 0 {
		t.Fatalf("glm %+v", glm)
	}
	free, ok := c.Lookup("vendor/small:free")
	if !ok || free.Name != "openrouter/vendor/small:free" || *free.Pricing.Input != 0 || !free.Supports.Images {
		t.Fatalf("free %+v", free)
	}
	if _, ok := c.Lookup("vendor/safety:free"); ok {
		t.Fatal("a free model without tools is in the catalog")
	}
}

// A running Lux's model list adds every model it sells that the Lux
// catalog does not name, in either spelling of a version, priced as the
// list quotes it: windows from OpenRouter where it lists the model, else
// from the list; an Anthropic model also answers to its version written
// with hyphens, and a provider Topos has no family of goes to the Chat
// dialect.
func TestRunAddsWhatALuxSells(t *testing.T) {
	dir := t.TempDir()
	lux := filepath.Join(dir, "models")
	if err := os.Mkdir(lux, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(lux, "anthropic__claude-opus-5.yaml"), model("anthropic/claude-opus-5", "anthropic", "claude-opus-5",
		"  pricing:\n    per: 1000000\n    input: \"5\"\n    output: \"25\"\n"))
	write(t, filepath.Join(lux, "anthropic__claude-sonnet-4.yaml"), model("anthropic/claude-sonnet-4", "anthropic", "claude-sonnet-4", ""))
	or := filepath.Join(dir, "openrouter.json")
	write(t, or, `{"data":[
 {"id":"anthropic/claude-opus-5","context_length":1000000,"top_provider":{"max_completion_tokens":128000}},
 {"id":"anthropic/claude-opus-5.5","context_length":1000000,"top_provider":{"max_completion_tokens":128000},"architecture":{"input_modalities":["text","image"]},"supported_parameters":["tools","reasoning"]}
]}`)
	served := filepath.Join(dir, "served.json")
	write(t, served, `{"data":[
 {"id":"anthropic/claude-opus-5","max_input_tokens":1,"max_tokens":1,"pricing":{"currency":"USD","per":1000000,"input":"9","output":"9"}},
 {"id":"anthropic/claude-sonnet-4","max_input_tokens":200000,"max_tokens":64000},
 {"id":"anthropic/claude-opus-5.5","max_input_tokens":1000000,"max_tokens":128000,"input_modalities":["text","image"],"pricing":{"currency":"USD","per":1000000,"input":"4","output":"20","cached_input":"0.2","cache_write":"5"}},
 {"id":"x-ai/grok-9","context_window":500000,"max_output_tokens":64000,"pricing":{"currency":"EUR","per":1000000,"input":"1","output":"2"}}
]}`)
	// The catalog being replaced knew what claude-sonnet-4 takes, which
	// OpenRouter no longer says.
	previous := filepath.Join(dir, "previous.json")
	write(t, previous, `{"models":[{"name":"anthropic/claude-sonnet-4","aliases":["claude-sonnet-4"],"supports":{"thinking":true,"images":true}}]}`)
	out := filepath.Join(dir, "catalog.json")
	if err := run(lux, served, previous, or, "2026-10-21", out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var c models.Catalog
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Models) != 4 || !strings.Contains(c.Source, "served model list") {
		t.Fatalf("catalog of %d from %q, want the Lux catalog's two models and the two only the list sells", len(c.Models), c.Source)
	}
	// A Lux catalog model OpenRouter does not list takes the list's windows
	// and keeps what the catalog it replaces said it supports.
	if sonnet, _ := c.Lookup("claude-sonnet-4"); sonnet.InputWindow != 200_000 || sonnet.MaxOutputTokens != 64_000 || !sonnet.Supports.Thinking || !sonnet.Supports.Images {
		t.Fatalf("the Lux catalog's sonnet %+v, want the served list's windows and the previous catalog's supports", sonnet)
	}
	// A model neither OpenRouter nor the catalog being replaced knows is
	// known to support nothing.
	if grok, _ := c.Lookup("grok-9"); grok.Supports != (models.Supports{}) {
		t.Fatalf("grok %+v", grok.Supports)
	}
	// The Lux catalog's own entry keeps its prices.
	if opus, _ := c.Lookup("claude-opus-5"); *opus.Pricing.Input != 5_000_000 || opus.InputWindow != 1_000_000 {
		t.Fatalf("the Lux catalog's opus %+v", opus)
	}
	for _, name := range []string{"anthropic/claude-opus-5.5", "anthropic/claude-opus-5-5", "claude-opus-5-5", "claude-opus-5.5"} {
		e, ok := c.Lookup(name)
		if !ok || e.Name != "anthropic/claude-opus-5.5" || e.Family != models.FamilyAnthropic || e.Dialect != "anthropic-messages" {
			t.Fatalf("%s = %+v, %v", name, e, ok)
		}
		if e.InputWindow != 1_000_000 || e.MaxOutputTokens != 128_000 || !e.Supports.Images || !e.Supports.Thinking ||
			*e.Pricing.Input != 4_000_000 || *e.Pricing.Output != 20_000_000 || *e.Pricing.CacheRead != 200_000 || *e.Pricing.CacheWrite != 5_000_000 {
			t.Fatalf("%s figures %+v pricing %+v", name, e, e.Pricing)
		}
	}
	grok, ok := c.Lookup("grok-9")
	if !ok || grok.Family != models.FamilyOther || grok.Dialect != "openai-chat" || grok.InputWindow != 500_000 || grok.MaxOutputTokens != 64_000 || grok.Pricing != nil {
		t.Fatalf("grok %+v, want the list's windows and no prices quoted in another currency", grok)
	}

	write(t, served, `{"data":[{"id":"no-provider"}]}`)
	if err := run(lux, served, "", or, "d", out); err == nil {
		t.Fatal("ran over a model list naming a model without its provider")
	}
	write(t, served, `{"data":[{"id":"a/b","pricing":{"per":1000000,"input":"one"}}]}`)
	if err := run(lux, served, "", or, "d", out); err == nil {
		t.Fatal("ran over a model list with a price that is not a decimal")
	}
	if err := run(lux, filepath.Join(dir, "missing.json"), "", or, "d", out); err == nil {
		t.Fatal("ran without the model list it was given")
	}
	if err := run(lux, "", filepath.Join(dir, "missing.json"), or, "d", out); err == nil {
		t.Fatal("ran without the previous catalog it was given")
	}
	write(t, previous, "{")
	if err := run(lux, "", previous, or, "d", out); err == nil {
		t.Fatal("ran over an undecodable previous catalog")
	}
}

func TestRunRefusesBadInput(t *testing.T) {
	dir := t.TempDir()
	or := filepath.Join(dir, "openrouter.json")
	write(t, or, openRouter)
	if err := run("", "", "", or, "d", filepath.Join(dir, "out")); err == nil {
		t.Fatal("ran without -lux")
	}
	if err := run(dir, "", "", filepath.Join(dir, "missing.json"), "d", filepath.Join(dir, "out")); err == nil {
		t.Fatal("ran without the OpenRouter list")
	}
	bad := filepath.Join(dir, "bad.json")
	write(t, bad, "{")
	if err := run(dir, "", "", bad, "d", filepath.Join(dir, "out")); err == nil {
		t.Fatal("ran over an undecodable OpenRouter list")
	}
	for name, body := range map[string]string{
		"a.yaml": "metadata: [",
		"b.yaml": "kind: Model\nmetadata:\n  name: x\nspec: {}\n",
		"c.yaml": model("x", "openai", "x", "  pricing:\n    per: 1000\n    input: \"1\"\n"),
		"d.yaml": model("x", "openai", "x", "  pricing:\n    per: 1000000\n    input: \"one\"\n"),
	} {
		sub := filepath.Join(dir, strings.TrimSuffix(name, ".yaml"))
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(sub, name), body)
		if err := run(sub, "", "", or, "d", filepath.Join(dir, "out")); err == nil {
			t.Fatalf("%s: ran over a bad manifest", name)
		}
	}
}

func TestCLIExitCodes(t *testing.T) {
	var stderr strings.Builder
	if code := cli([]string{"-nope"}, &stderr); code != 2 {
		t.Fatalf("a bad flag exits %d", code)
	}
	if code := cli(nil, &stderr); code != 1 || !strings.Contains(stderr.String(), "required") {
		t.Fatalf("missing flags exit %d: %s", code, stderr.String())
	}
	dir := t.TempDir()
	or := filepath.Join(dir, "openrouter.json")
	write(t, or, openRouter)
	if code := cli([]string{"-lux", dir, "-openrouter", or, "-date", "2026-09-27", "-out", filepath.Join(dir, "c.json")}, &stderr); code != 0 {
		t.Fatalf("a good run exits %d", code)
	}
}
