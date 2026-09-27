// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arch

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const module = "latere.ai/x/topos"

// root is the module root, found from this file rather than from the
// working directory, so the test reads the same tree wherever it runs.
func root(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller information")
	}
	dir := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod: %v", dir, err)
	}
	return dir
}

// goList runs go list with the given arguments at the module root and
// returns its lines.
func goList(t *testing.T, dir string, args ...string) []string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// exported are the trees of spec 001's package table that sit at the
// module root and are imported by embedders. Everything else is a role,
// a tool, a test, or an example under cmd, internal, test, tools or
// examples.
var exported = []string{"session", "harness", "prompts", "models", "machine", "runner", "memory", "manifest", "client", "authorizer"}

var rootDirs = append([]string{"cmd", "internal", "test", "tools", "examples"}, exported...)

// TestPackagesSitInTheirTrees is the first half of spec 001's package
// rule: every package of the module sits in one of the exported trees or
// under cmd, internal, test, tools or examples, and none at the root.
func TestPackagesSitInTheirTrees(t *testing.T) {
	dir := root(t)
	for _, pkg := range goList(t, dir, "./...") {
		rel := strings.TrimPrefix(strings.TrimPrefix(pkg, module), "/")
		first, _, _ := strings.Cut(rel, "/")
		if rel == "" || !slices.Contains(rootDirs, first) {
			t.Errorf("package %s sits outside the trees of spec 001: %s", pkg, strings.Join(rootDirs, ", "))
		}
	}
}

// dialing are the standard-library packages that open a connection. A
// package that imports one can dial the network.
var dialing = []string{"net", "net/http", "net/rpc", "net/smtp", "crypto/tls"}

// pure are the root trees that dial nothing (spec 001, invariant 13): the
// session schema, the harness, the texts a model reads, the manifest
// kinds and the action vocabulary are pure over the interfaces they are
// handed.
var pure = []string{"session", "harness", "prompts", "manifest", "authorizer"}

// TestRootPackagesDialNothing is invariant 13 of spec 001: no package in
// a pure tree imports a package that opens a connection, directly or
// through a dependency. A tree that does not exist yet is skipped by
// name, so the rule holds from the scaffold and binds each tree the day
// it lands.
func TestRootPackagesDialNothing(t *testing.T) {
	dir := root(t)
	for _, tree := range pure {
		if _, err := os.Stat(filepath.Join(dir, tree)); os.IsNotExist(err) {
			continue
		}
		for _, pkg := range goList(t, dir, "./"+tree+"/...") {
			for _, dep := range goList(t, dir, "-deps", pkg) {
				if slices.Contains(dialing, dep) {
					t.Errorf("%s reaches %s; the %s tree dials nothing (spec 001)", pkg, dep, tree)
				}
			}
		}
	}
}

// TestPromptsImportNothingOfTheModule holds the prompts tree at the bottom
// of the module's graph (spec 001): the session fold, the harness and the
// tools import it, so it imports no package of this module.
func TestPromptsImportNothingOfTheModule(t *testing.T) {
	dir := root(t)
	for _, pkg := range goList(t, dir, "./prompts/...") {
		for _, dep := range goList(t, dir, "-deps", pkg) {
			if dep != pkg && (dep == module || strings.HasPrefix(dep, module+"/")) {
				t.Errorf("%s imports %s; the prompts tree imports nothing of %s", pkg, dep, module)
			}
		}
	}
}
