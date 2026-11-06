// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"latere.ai/x/topos/models"
)

// benchRegistry is the registry of the built-in tools, as a session offers
// them to the model.
func benchRegistry(b *testing.B) *Registry {
	b.Helper()
	r := NewRegistry()
	for _, tool := range Builtins() {
		if err := r.AddBuiltin(tool); err != nil {
			b.Fatal(err)
		}
	}
	return r
}

// benchProse returns n bytes of deterministic source-like text with
// characters JSON escapes.
func benchProse(n int) string {
	const unit = "func main() {\n\tfmt.Println(\"hello, <world> & friends\")\n}\n"
	return strings.Repeat(unit, n/len(unit)+1)[:n]
}

// BenchmarkJSONToolInputValidate is every tool call of a session passing
// the registry's check: the model's arguments decoded and matched against
// the tool's schema before the call is scored or run.
func BenchmarkJSONToolInputValidate(b *testing.B) {
	r := benchRegistry(b)
	for _, s := range []struct {
		name string
		tool string
		n    int
	}{{"small", NameRead, 0}, {"medium", NameWrite, 10 << 10}, {"large", NameWrite, 200 << 10}} {
		b.Run(s.name, func(b *testing.B) {
			var in []byte
			var err error
			if s.n == 0 {
				in, err = json.Marshal(map[string]any{"path": "session/event.go", "offset": 120, "limit": 80}, models.ToProvider)
			} else {
				in, err = json.Marshal(map[string]any{"path": "internal/server/generated.go", "content": benchProse(s.n)}, models.ToProvider)
			}
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(in)))
			for b.Loop() {
				if _, res := r.Validate(s.tool, jsontext.Value(in)); res != nil {
					b.Fatal("refused")
				}
			}
		})
	}
}

// BenchmarkJSONToolInputInvalidValidate is a model's cut-off arguments
// refused by the registry, the path a runaway call takes.
func BenchmarkJSONToolInputInvalidValidate(b *testing.B) {
	r := benchRegistry(b)
	in := []byte(`{"path":"internal/server/generated.go","content":"` + benchProse(10<<10))
	b.ReportAllocs()
	b.SetBytes(int64(len(in)))
	for b.Loop() {
		if _, res := r.Validate(NameWrite, jsontext.Value(in)); res == nil {
			b.Fatal("accepted")
		}
	}
}

// BenchmarkJSONToolDefinitionsMarshal is the tool list a model request
// carries, encoded on every step of every turn.
func BenchmarkJSONToolDefinitionsMarshal(b *testing.B) {
	r := benchRegistry(b)
	defs := r.Definitions()
	enc, err := json.Marshal(defs, models.ToProvider)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(enc)))
	for b.Loop() {
		if _, err := json.Marshal(defs, models.ToProvider); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkJSONSchemaCompile is a tool's input schema compiled when the
// tool is registered, once per tool per session.
func BenchmarkJSONSchemaCompile(b *testing.B) {
	defs := benchRegistry(b).Definitions()
	var total int
	for _, d := range defs {
		total += len(d.InputSchema)
	}
	b.ReportAllocs()
	b.SetBytes(int64(total))
	for b.Loop() {
		for _, d := range defs {
			if _, err := CompileSchema(d.InputSchema); err != nil {
				b.Fatal(err)
			}
		}
	}
}
