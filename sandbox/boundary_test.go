// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package sandbox_test

// Package boundary: this test asserts that the sandbox package itself — the
// interface and shared types — does NOT import the Cella backend package.
// Upstream code that imports sandbox must remain unaware that Cella exists, so
// every dependency runs through the interface rather than the client.
//
// It checks the weaker but sufficient condition: no non-test file directly in
// sandbox/ may import sandbox/cella. The forward direction — that cella DOES
// import sandbox — is validated naturally by the compiler when cella/provider.go
// compiles.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cellaPkg = "latere.ai/x/topos/sandbox/cella"

// TestSandboxPackageDoesNotImportCella parses every .go file directly in the
// sandbox/ directory (excluding subdirectories) and asserts that none of them
// import the cella package path.
func TestSandboxPackageDoesNotImportCella(t *testing.T) {
	// Resolve the directory containing this test file.
	// os.Getwd() during testing is the package directory.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}

	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() {
			// Do not recurse; only check the top-level sandbox package.
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			// imp.Path.Value includes surrounding quotes.
			imported := strings.Trim(imp.Path.Value, `"`)
			if imported == cellaPkg || strings.HasPrefix(imported, cellaPkg+"/") {
				t.Errorf("file %s imports %q: the sandbox package must not depend on the cella backend", name, imported)
			}
		}
	}
}
