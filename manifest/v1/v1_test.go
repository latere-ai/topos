// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestAToolIsANameOrAnObject(t *testing.T) {
	var spec AgentSpec
	in := `{"tools":["read",{"name":"ask","client":true,"description":"Ask the person.","inputSchema":{"type":"object"}},{"name":"bash","outputLimit":4096}]}`
	if err := json.Unmarshal([]byte(in), &spec); err != nil {
		t.Fatal(err)
	}
	want := []Tool{
		{Name: "read"},
		{Name: "ask", Client: true, Description: "Ask the person.", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "bash", OutputLimit: 4096},
	}
	if !reflect.DeepEqual(spec.Tools, want) {
		t.Fatalf("tools %+v", spec.Tools)
	}
	b, err := json.Marshal(spec.Tools)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `["read",{"name":"ask","client":true,"description":"Ask the person.","inputSchema":{"type":"object"}},{"name":"bash","outputLimit":4096}]` {
		t.Fatalf("round trip %s", got)
	}
	for _, bad := range []string{`[1]`, `[{"name":1}]`, `[""`} {
		var tools []Tool
		if err := json.Unmarshal([]byte(bad), &tools); err == nil {
			t.Fatalf("%s decoded", bad)
		}
	}
	var tool Tool
	if err := tool.UnmarshalJSON([]byte(" true ")); err == nil || !strings.Contains(err.Error(), "name or an object") {
		t.Fatalf("a boolean tool: %v", err)
	}
}

func TestTheResolvedFormWritesFixedDefaultsAndOmitsTheRest(t *testing.T) {
	a := Agent{APIVersion: APIVersion, Kind: KindAgent, Metadata: ObjectMeta{Name: "reviewer"}}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{`"apiVersion":"topos.latere.ai/v1"`, `"kind":"Agent"`, `"instructions":""`, `"tools":null`, `"identity":""`, `"threads":{"maxDepth":null,"maxConcurrent":null}`, `"limits":{"turnTimeout":"","maxAge":""}`} {
		if !strings.Contains(got, want) {
			t.Fatalf("%s lacks %s", got, want)
		}
	}
	for _, absent := range []string{"status", "budget", "description", "subagents", "resources", "advisor"} {
		if strings.Contains(got, `"`+absent+`"`) {
			t.Fatalf("%s writes %s", got, absent)
		}
	}
}
