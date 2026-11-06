// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package publish

import (
	"encoding/json/jsontext"
	"testing"
)

// TestACallsInputIsReadAsTheModelWroteIt: a member name in another letter
// case, a repeated member and bytes that are not UTF-8 are read.
func TestACallsInputIsReadAsTheModelWroteIt(t *testing.T) {
	in, err := decode(jsontext.Value(`{"Path":"site","path":"site2","NAME":"Two` + "\xff" + `","release":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if in.Path != "site2" || in.Name != "Two�" || !in.Release {
		t.Fatalf("read %+v", in)
	}
	if in, err := decode(nil); err != nil || in != (input{}) {
		t.Fatalf("an empty input read %+v, %v", in, err)
	}
}
