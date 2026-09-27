// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"strings"
	"testing"
)

// TestCheckRepository: a repository is a URL of an allowed scheme that
// names its host, with no credential, query or fragment, and a ref git
// reads as a name; a refusal of a URL holding a credential does not
// quote it.
func TestCheckRepository(t *testing.T) {
	for _, c := range []struct {
		r    Resource
		fail string
	}{
		{Resource{URL: "https://code.example/org/app.git", Ref: "release/1.2"}, ""},
		{Resource{URL: "https://code.example/org/app.git", Ref: "0123abcd"}, ""},
		{Resource{URL: "http://code.example/org/app.git"}, "is not https"},
		{Resource{URL: "https:///app.git"}, "names no host"},
		{Resource{URL: "https://user:pw@code.example/app.git"}, "holds a credential"},
		{Resource{URL: "http://user:pw@code.example/app.git"}, "holds a credential"},
		{Resource{URL: "https://code.example/app.git?x=1"}, "query or a fragment"},
		{Resource{URL: "::"}, "does not parse"},
		{Resource{URL: "https://code.example/app.git", Ref: "--upload-pack=x"}, "not a branch"},
		{Resource{URL: "https://code.example/app.git", Ref: "main..dev"}, "not a branch"},
		{Resource{URL: "https://code.example/app.git", Ref: "a b"}, "not a branch"},
	} {
		err := CheckRepository(c.r, "https")
		if c.fail == "" && err != nil || c.fail != "" && (err == nil || !strings.Contains(err.Error(), c.fail)) {
			t.Errorf("%+v: %v, want %q", c.r, err, c.fail)
		}
		if err != nil && strings.Contains(err.Error(), "pw@") {
			t.Errorf("%+v: the refusal quotes the credential: %v", c.r, err)
		}
	}
}
