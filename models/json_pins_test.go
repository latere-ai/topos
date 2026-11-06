// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package models

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
)

// TestToProviderWritesTheRequestAProviderReads pins the bytes of a
// request: sorted keys, null for nil lists and maps, an empty object for
// a loss report, and model text passed through.
func TestToProviderWritesTheRequestAProviderReads(t *testing.T) {
	type body struct {
		Tools []string          `json:"tools"`
		Meta  map[string]string `json:"meta"`
		Order map[string]int    `json:"order"`
		Text  string            `json:"text"`
	}
	got, err := json.Marshal(body{Order: map[string]int{"b": 1, "a": 2}, Text: "<x> & \xff"}, ToProvider)
	want := "{\"tools\":null,\"meta\":null,\"order\":{\"a\":2,\"b\":1},\"text\":\"<x> & �\"}"
	if err != nil || string(got) != want {
		t.Fatalf("%s, %v\nwant %s", got, err, want)
	}
	got, err = json.Marshal(struct {
		Loss ir.Loss
		Args jsontext.Value
	}{Args: jsontext.Value(`{"a":1,"a":2}`)}, ToProvider)
	if err != nil || string(got) != `{"Loss":{},"Args":{"a":1,"a":2}}` {
		t.Fatalf("%s, %v", got, err)
	}
}

// TestFromProviderReadsWhatAProviderSent: repeated names (the last
// wins), invalid UTF-8 and any letter case of a name are read.
func TestFromProviderReadsWhatAProviderSent(t *testing.T) {
	var v struct {
		Name string `json:"name"`
		N    int
	}
	if err := json.Unmarshal([]byte("{\"NAME\":\"a\",\"name\":\"b\xff\",\"n\":4}"), &v, FromProvider); err != nil || v.Name != "b�" || v.N != 4 {
		t.Fatalf("%+v, %v", v, err)
	}
	if err := json.Unmarshal([]byte(`{"name":"a","name":"b"}`), &v); err == nil {
		t.Fatal("repeated names read without the option")
	}
}
