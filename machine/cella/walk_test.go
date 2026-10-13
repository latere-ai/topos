// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"testing"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/test/stubs/cellastub"
)

// TestASandboxWalksItsWorkingDirectory: the helper walks the sandbox's
// working directory where the files are, the deny-list's .env hidden and
// a directory the request leaves out skipped, and a walk Cella cannot
// start is an error (spec 073).
func TestASandboxWalksItsWorkingDirectory(t *testing.T) {
	f := open(t)
	tree(t, f.ws())
	q := machine.WalkRequest{LeftOut: []string{"build"}, MaxFiles: 100, MaxBytes: 1 << 20, MaxFileBytes: 1 << 20, MaxListed: 100}
	res, err := f.m.Walk(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, file := range res.Files {
		if file.SHA256 == "" || file.Mode != 0o644 {
			t.Errorf("%s: %+v", file.Path, file)
		}
		paths = append(paths, file.Path)
	}
	want := []string{".gitignore", "lib/lib.go", "lib/lib_test.go", "main.go", "notes.txt"}
	if len(paths) != len(want) {
		t.Fatalf("the walk %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("the walk %v, want %v", paths, want)
		}
	}
	f.stub.Fail(cellastub.OpSession, cellastub.Failure{Status: 503, Code: "driver_unavailable"})
	if _, err := f.m.Walk(t.Context(), q); err == nil {
		t.Error("a walk Cella could not start was answered")
	}
}
