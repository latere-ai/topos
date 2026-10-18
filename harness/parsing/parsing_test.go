// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package parsing

import (
	"slices"
	"strings"
	"testing"
)

// TestSelect: a selection names pages and closed and open ranges, joined
// by commas with space allowed; pages past the document are left out; a
// malformed one is refused; Format writes runs back, and Next names the
// pages after the last shown.
func TestSelect(t *testing.T) {
	for _, c := range []struct {
		expr  string
		total int
		want  []int
	}{
		{"1-5", 12, []int{1, 2, 3, 4, 5}},
		{" 1 - 3 , 7 ", 12, []int{1, 2, 3, 7}},
		{"9-", 12, []int{9, 10, 11, 12}},
		{"3,1-2,2", 12, []int{1, 2, 3}},
		{"1-5", 3, []int{1, 2, 3}},
		{"1-999999999", 4, []int{1, 2, 3, 4}},
		{"20-", 12, []int{}},
		{"13", 12, []int{}},
	} {
		got, err := Select(c.expr, c.total)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("Select(%q, %d) = %v, %v; want %v", c.expr, c.total, got, err, c.want)
		}
	}
	for _, expr := range []string{"", " ", "0", "0-3", "a", "3-1", "1,,2", "-3", "1-x", "1-2-3", strings.Repeat("1,", MaxSelection)} {
		if _, err := Select(expr, 10); err == nil {
			t.Errorf("Select(%q) was taken", expr)
		}
		if CheckPages(expr) == nil {
			t.Errorf("CheckPages(%q) was taken", expr)
		}
	}
	if err := CheckPages(DefaultSelection); err != nil || DefaultSelection != "1-5" {
		t.Fatalf("the default selection %q: %v", DefaultSelection, err)
	}
	for _, c := range []struct {
		pages []int
		want  string
	}{
		{nil, ""}, {[]int{4}, "4"}, {[]int{1, 2, 3, 7}, "1-3,7"}, {[]int{1, 3, 5, 6}, "1,3,5-6"},
	} {
		if got := Format(c.pages); got != c.want {
			t.Errorf("Format(%v) = %q; want %q", c.pages, got, c.want)
		}
	}
	for _, c := range []struct {
		shown []int
		total int
		want  string
	}{
		{[]int{1, 2, 3, 4, 5}, 12, "6-10"}, {[]int{1, 2, 3, 4, 5}, 7, "6-7"}, {[]int{1, 2, 3, 4, 5}, 6, "6"},
		{[]int{1, 2, 3}, 3, ""}, {nil, 3, ""},
	} {
		if got := Next(c.shown, c.total); got != c.want {
			t.Errorf("Next(%v, %d) = %q; want %q", c.shown, c.total, got, c.want)
		}
	}
	for _, err := range []error{&Refused{Status: 403, Code: "forbidden", Message: "m"}, &Failed{State: "failed", Code: "c", Detail: "d"}, &PastEnd{Pages: 3}} {
		if err.Error() == "" {
			t.Errorf("%T says nothing", err)
		}
	}
}
