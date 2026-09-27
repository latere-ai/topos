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
 {"id":"anthropic/claude-opus-5","context_length":1000000,"top_provider":{"max_completion_tokens":128000},"supported_parameters":["tools","reasoning"]},
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
	write(t, filepath.Join(lux, "openai__gpt-5-6.yaml"), model("openai/gpt-5-6", "openai", "gpt-5.6", ""))
	write(t, filepath.Join(lux, "gemini__flash.yaml"), model("gemini/flash", "gemini", "flash", ""))
	write(t, filepath.Join(lux, "zhipu__glm.yaml"), model("zhipu/glm", "zhipu", "glm", ""))
	or := filepath.Join(dir, "openrouter.json")
	write(t, or, openRouter)
	out := filepath.Join(dir, "catalog.json")
	if err := run(lux, or, "2026-09-27", out); err != nil {
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
	if !strings.Contains(c.Source, "2026-09-27") || len(c.Models) != 4 {
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
	if _, ok := c.Lookup("gemini/flash"); ok {
		t.Fatal("a Gemini model, which no codec speaks, is in the catalog")
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

func TestRunRefusesBadInput(t *testing.T) {
	dir := t.TempDir()
	or := filepath.Join(dir, "openrouter.json")
	write(t, or, openRouter)
	if err := run("", or, "d", filepath.Join(dir, "out")); err == nil {
		t.Fatal("ran without -lux")
	}
	if err := run(dir, filepath.Join(dir, "missing.json"), "d", filepath.Join(dir, "out")); err == nil {
		t.Fatal("ran without the OpenRouter list")
	}
	bad := filepath.Join(dir, "bad.json")
	write(t, bad, "{")
	if err := run(dir, bad, "d", filepath.Join(dir, "out")); err == nil {
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
		if err := run(sub, or, "d", filepath.Join(dir, "out")); err == nil {
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
