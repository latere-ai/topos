// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
)

// TestAToolKeepsItsTwoForms pins the bytes of spec.tools: the bare name
// when only the name is set, an object otherwise with a false flag and a
// zero limit left out.
func TestAToolKeepsItsTwoForms(t *testing.T) {
	tools := []Tool{
		{Name: "bash"},
		{Name: "ask", Client: true, Description: "d", InputSchema: jsontext.Value(`{"type":"object"}`)},
		{Name: "read", OutputLimit: 10},
	}
	got, err := json.Marshal(tools)
	want := `["bash",{"name":"ask","client":true,"description":"d","inputSchema":{"type":"object"}},{"name":"read","outputLimit":10}]`
	if err != nil || string(got) != want {
		t.Fatalf("%s, %v\nwant %s", got, err, want)
	}
	var back []Tool
	if err := json.Unmarshal(got, &back); err != nil || len(back) != 3 || back[0].Name != "bash" || back[1].Description != "d" || back[2].OutputLimit != 10 {
		t.Fatalf("%+v, %v", back, err)
	}
	if err := json.Unmarshal([]byte(`[1]`), &back); err == nil {
		t.Fatal("a number is not a tool")
	}
}
