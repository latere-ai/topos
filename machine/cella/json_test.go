// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"encoding/json/v2"
	"testing"
)

// TestTheWireOfTheSandboxAndTheHelper pins the bytes of a request to
// Cella (sorted keys, null for nil lists and maps) and of the helper's
// protocol (file names that are not UTF-8 survive, the helper's repeated
// names are read).
func TestTheWireOfTheSandboxAndTheHelper(t *testing.T) {
	type body struct {
		List []string          `json:"list"`
		Map  map[string]string `json:"map"`
		Path string            `json:"path"`
	}
	got, err := json.Marshal(body{Path: "<a>"}, toCella)
	if err != nil || string(got) != `{"list":null,"map":null,"path":"<a>"}` {
		t.Fatalf("%s, %v", got, err)
	}
	got, err = json.Marshal(body{Path: "a\xffb"}, toHelper)
	if err != nil || string(got) != "{\"list\":[],\"map\":{},\"path\":\"a�b\"}" {
		t.Fatalf("%q, %v", got, err)
	}
	r, err := decodeResponse([]byte("{\"done\":false,\"done\":true,\"error\":{\"kind\":\"k\",\"message\":\"m\xff\"}}\n"))
	if err != nil || !r.Done || r.Error == nil || r.Error.Message != "m�" {
		t.Fatalf("%+v, %v", r, err)
	}
}
