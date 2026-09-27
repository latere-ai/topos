// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arch

import (
	"bufio"
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
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

// contracts are the shared packages a pure tree may import although
// they also hold a client: the authorizer tree publishes the value types
// of latere.ai/x/pkg/authz, whose Client is the one toposd dials with.
// The walk below does not descend into a contract, so a dialing package
// the tree imports itself, or reaches through anything else, still
// fails.
var contracts = map[string][]string{"authorizer": {"latere.ai/x/pkg/authz"}}

// TestRootPackagesDialNothing is invariant 13 of spec 001: no package in
// a pure tree imports a package that opens a connection, directly or
// through a dependency other than its tree's contracts. A tree that does
// not exist yet is skipped by name, so the rule holds from the scaffold
// and binds each tree the day it lands.
func TestRootPackagesDialNothing(t *testing.T) {
	dir := root(t)
	for _, tree := range pure {
		if _, err := os.Stat(filepath.Join(dir, tree)); os.IsNotExist(err) {
			continue
		}
		for _, pkg := range goList(t, dir, "./"+tree+"/...") {
			for _, dep := range reached(goList(t, dir, "-deps", "-f", "{{.ImportPath}} {{join .Imports \" \"}}", pkg), pkg, func(p string) bool { return slices.Contains(contracts[tree], p) }) {
				if slices.Contains(dialing, dep) {
					t.Errorf("%s reaches %s; the %s tree dials nothing (spec 001)", pkg, dep, tree)
				}
			}
		}
	}
}

// reached walks the import graph go list printed, one "path imports..."
// line per package, from pkg, and does not enter a package stop names.
func reached(graph []string, pkg string, stop func(string) bool) []string {
	edges := map[string][]string{}
	for _, line := range graph {
		fields := strings.Fields(line)
		edges[fields[0]] = fields[1:]
	}
	seen := map[string]bool{pkg: true}
	queue := []string{pkg}
	var out []string
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, next := range edges[p] {
			if seen[next] || stop(next) {
				continue
			}
			seen[next] = true
			out = append(out, next)
			queue = append(queue, next)
		}
	}
	return out
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

// transport is the family's instrumented HTTP transport, which every
// outbound call of the module goes through (spec 001, Dependencies).
// It holds an exporter's client of its own and is allowed to every
// package below.
const transport = "latere.ai/x/pkg/otel"

// clients are the packages of spec 001 that dial one base URL their
// caller hands them, each with the network clients it may reach besides
// the transport: a Cella sandbox through Cella's client, a model through
// llmdialect's codecs, toposd through nothing but the transport.
var clients = map[string][]string{
	"machine/cella":  {"latere.ai/x/cella/client"},
	"models/dialect": {"latere.ai/x/pkg/llmdialect"},
	"client":         {},
}

// within reports whether pkg is one of roots or a package beneath one.
func within(pkg string, roots []string) bool {
	for _, r := range roots {
		if pkg == r || strings.HasPrefix(pkg, r+"/") {
			return true
		}
	}
	return false
}

// TestClientsKeepToTheirAllowLists is invariant 13 of spec 001 for the
// packages that dial: each reaches no network client but its allow list
// and the transport, constructs one HTTP client, and reaches nothing
// under internal/. A network client is a package outside the standard
// library that imports a dialing package; the walk does not enter an
// allowed one, so a client reached any other way fails. A tree that
// does not exist yet is skipped by name and binds the day it lands.
func TestClientsKeepToTheirAllowLists(t *testing.T) {
	dir := root(t)
	for rel, allowed := range clients {
		if _, err := os.Stat(filepath.Join(dir, rel)); os.IsNotExist(err) {
			continue
		}
		pkg := module + "/" + rel
		allow := append([]string{transport}, allowed...)
		lines := goList(t, dir, "-deps", "-f", "{{.ImportPath}} {{.Standard}} {{join .Imports \" \"}}", pkg)
		standard := map[string]bool{}
		dials := map[string]bool{}
		graph := make([]string, 0, len(lines))
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				t.Fatalf("go list printed %q", line)
			}
			standard[fields[0]] = fields[1] == "true"
			for _, imp := range fields[2:] {
				if slices.Contains(dialing, imp) {
					dials[fields[0]] = true
				}
			}
			graph = append(graph, strings.Join(append(fields[:1:1], fields[2:]...), " "))
		}
		for _, dep := range reached(graph, pkg, func(p string) bool { return within(p, allow) }) {
			if !standard[dep] && dials[dep] {
				t.Errorf("%s reaches the network client %s; it may reach %s (spec 001)", pkg, dep, strings.Join(allow, ", "))
			}
		}
		for _, dep := range goList(t, dir, "-deps", pkg) {
			if strings.HasPrefix(dep, module+"/internal/") {
				t.Errorf("%s reaches %s; a package an embedder imports reaches nothing under internal/ (spec 001)", pkg, dep)
			}
		}
		if n := httpClients(t, filepath.Join(dir, rel)); n != 1 {
			t.Errorf("%s constructs %d HTTP clients; it sends through one (spec 001)", pkg, n)
		}
	}
}

// httpClients counts the HTTP clients the non-test files of one package
// directory construct: an http.Client literal, or the transport's
// otel.HTTPClient.
func httpClients(t *testing.T, dir string) int {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(node ast.Node) bool {
			switch x := node.(type) {
			case *ast.CompositeLit:
				if selects(x.Type, "http", "Client") {
					n++
				}
			case *ast.CallExpr:
				if selects(x.Fun, "otel", "HTTPClient") {
					n++
				}
			}
			return true
		})
	}
	return n
}

// selects reports whether e is the selector pkg.name.
func selects(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}
