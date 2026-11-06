// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"testing"
)

// TestToolArgumentsAreReadAsAModelWritesThem: the arguments of a call are
// model output, so a name in another case and a repeated name (the last
// wins) are read, as is invalid UTF-8 in a value.
func TestToolArgumentsAreReadAsAModelWritesThem(t *testing.T) {
	if got := fetchHost(jsontext.Value(`{"URL":"https://Example.com:8443/x"}`)); got != "example.com" {
		t.Errorf("a name in another case: %q", got)
	}
	if got := fetchHost(jsontext.Value(`{"url":"http://a.test","url":"http://b.test"}`)); got != "b.test" {
		t.Errorf("a repeated name: %q", got)
	}
	if got := patternSubject("bash", jsontext.Value("{\"command\":\"ls \xff\"}")); got != "ls �" {
		t.Errorf("invalid UTF-8: %q", got)
	}
}

// TestACallOfTheSameInputIsTheSameCall: two spellings of one input,
// other spacing, key order and the digits of a number kept, give the same
// identity, and a number past 2^53 is not rounded into its neighbor.
func TestACallOfTheSameInputIsTheSameCall(t *testing.T) {
	a := canonicalInput([]byte(`{ "b": 9007199254740993, "a": [1, 2.50] }`))
	b := canonicalInput([]byte(`{"a":[1,2.50],"b":9007199254740993}`))
	if string(a) != string(b) || string(a) != `{"a":[1,2.50],"b":9007199254740993}` {
		t.Fatalf("%s / %s", a, b)
	}
	if string(canonicalInput([]byte(`{"b":9007199254740992}`))) == string(canonicalInput([]byte(`{"b":9007199254740993}`))) {
		t.Fatal("two integers rounded alike")
	}
	if got := canonicalInput([]byte(`{"a":1} x`)); string(got) != `{"a":1} x` {
		t.Fatalf("text after the value: %s", got)
	}
}

// TestTheHashOfNoToolsIsTheHashOfNull: a request with no tools hashes the
// bytes null, as a session recorded it, so a resumed session does not read
// its unchanged tools as changed.
func TestTheHashOfNoToolsIsTheHashOfNull(t *testing.T) {
	got, err := toolsHash(nil)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("null"))
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("toolsHash(nil) = %s, want %s", got, want)
	}
}
