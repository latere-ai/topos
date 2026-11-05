// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"

	"latere.ai/x/pkg/authz"
)

// TestAdmitsReadsALabelFilterAsExcludingEveryObject: an allow with no
// filter admits every owner, owners admit by name, and a filter naming a
// label admits nothing because no list object carries one.
func TestAdmitsReadsALabelFilterAsExcludingEveryObject(t *testing.T) {
	for name, tc := range map[string]struct {
		filter *authz.Filter
		want   bool
	}{
		"no filter":           {nil, true},
		"owner named":         {&authz.Filter{Owners: []string{"alice"}}, true},
		"another owner":       {&authz.Filter{Owners: []string{"bob"}}, false},
		"owner and label":     {&authz.Filter{Owners: []string{"alice"}, Labels: map[string]string{"a": "b"}}, false},
		"empty filter":        {&authz.Filter{}, false},
		"label without owner": {&authz.Filter{Labels: map[string]string{"a": "b"}}, false},
	} {
		if got := admits(authz.Decision{Allow: true, Filter: tc.filter}, "alice"); got != tc.want {
			t.Errorf("%s: admits = %v, want %v", name, got, tc.want)
		}
	}
	if namesLabels(nil) || namesLabels(&authz.Filter{Owners: []string{"a"}}) || !namesLabels(&authz.Filter{Labels: map[string]string{"a": "b"}}) {
		t.Error("namesLabels misreads a filter")
	}
}
