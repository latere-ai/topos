package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitizeControls_NoControls(t *testing.T) {
	in := []byte(`{"k":"plain"}`)
	if got := sanitizeControls(in); !bytes.Equal(got, in) {
		t.Errorf("clean input mutated: got %q", got)
	}
}

func TestSanitizeControls_StringWithControl(t *testing.T) {
	in := []byte("{\"k\":\"\x01\"}")
	got := sanitizeControls(in)
	want := "\\u0001"
	if !strings.Contains(string(got), want) {
		t.Errorf("0x01 not escaped: got %q, want substring %q", got, want)
	}
}

func TestSanitizeControls_TabsNewlinesPreserved(t *testing.T) {
	in := []byte("{\"k\":\"\\tline\\nrest\\r\"}")
	got := sanitizeControls(in)
	var v map[string]string
	if err := json.Unmarshal(got, &v); err != nil {
		t.Errorf("sanitized output should still parse: %v\noutput=%q", err, got)
	}
}

func TestSanitizeControls_EscapeAfterBackslash(t *testing.T) {
	in := []byte("{\"k\":\"\\\\\\u0007\"}")
	got := sanitizeControls(in)
	if !bytes.Equal(got, in) {
		t.Errorf("escaped sequence rewritten: got %q want %q", got, in)
	}
}

func TestSanitizeControls_OutsideStringUntouched(t *testing.T) {
	in := []byte("\x01{\"k\":\"v\"}")
	got := sanitizeControls(in)
	if got[0] != 0x01 {
		t.Errorf("byte outside string was rewritten: %q", got)
	}
}

func TestDecodeJSONLine_PlainCleanInput(t *testing.T) {
	var v map[string]string
	if err := DecodeJSONLine([]byte(`{"a":"b"}`), &v); err != nil {
		t.Fatal(err)
	}
	if v["a"] != "b" {
		t.Errorf("got %v", v)
	}
}

func TestDecodeJSONLine_FallbackThroughSanitize(t *testing.T) {
	var v map[string]string
	if err := DecodeJSONLine([]byte("{\"a\":\"b\x01c\"}"), &v); err != nil {
		t.Errorf("should sanitize and decode: %v", err)
	}
}

func TestDecodeJSONLine_HardError(t *testing.T) {
	var v map[string]string
	if err := DecodeJSONLine([]byte("{not-json"), &v); err == nil {
		t.Error("expected error")
	}
}

func TestStreamJSON_HappyPath(t *testing.T) {
	body := []byte(`{"a":1}` + "\n" + `{"a":2}` + "\n" + `{"a":3}` + "\n")
	var got []int
	err := StreamJSON(bytes.NewReader(body), func(raw json.RawMessage) error {
		var x struct{ A int }
		_ = json.Unmarshal(raw, &x)
		got = append(got, x.A)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Errorf("got %v", got)
	}
}

func TestStreamJSON_SkipsBlankAndUndecodable(t *testing.T) {
	body := []byte("\n{garbage}\n{\"a\":1}\n\n")
	var hits int
	err := StreamJSON(bytes.NewReader(body), func(json.RawMessage) error {
		hits++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("hits: got %d, want 1", hits)
	}
}
