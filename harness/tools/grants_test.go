// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools_test

import (
	"slices"
	"testing"

	"latere.ai/x/topos/harness/tools"
)

func TestBuiltinGrantSelection(t *testing.T) {
	all := []string{"bash", "read_file", "write_file", "edit_file", "grep", "glob"}
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{"default", nil, all},
		{"empty", []string{}, []string{}},
		{"read family", []string{"read"}, []string{"read_file", "grep", "glob"}},
		{"write family", []string{"write"}, []string{"write_file", "edit_file"}},
		{"exec family", []string{"exec"}, []string{"bash"}},
		{"exact and duplicate", []string{"glob", "read_file", "read_file", "missing"}, []string{"read_file", "glob"}},
		{"unknown", []string{"missing", "delegate"}, []string{}},
		{"overlapping families", []string{"write_file", "write", "exec", "read"}, all},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := tools.BuiltinsFor(tc.in)
			if got := reg.Names(); got == nil || !slices.Equal(got, tc.want) {
				t.Fatalf("names = %#v, want %#v", got, tc.want)
			}
			for _, name := range all {
				if (reg.Get(name) != nil) != slices.Contains(tc.want, name) {
					t.Errorf("dispatch selection differs for %s", name)
				}
			}
		})
	}
}

func TestRegistrySelectNeverExpandsEmptyCapabilities(t *testing.T) {
	all := tools.Builtins()
	for _, names := range [][]string{nil, {}, {"missing"}} {
		reg := all.Select(names)
		if got := reg.Names(); got == nil || len(got) != 0 {
			t.Fatalf("empty capabilities produced %#v", got)
		}
	}
	names := all.Names()
	names[0] = "changed"
	if all.Names()[0] != "bash" || len(all.Defs()) != 6 {
		t.Fatal("selection or name mutation changed the source registry")
	}
}
