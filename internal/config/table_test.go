// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// readers are the trees whose TOPOS_* reads the configuration table of
// spec 002 must list: the server roles, the topos command, and the
// conformance suite. A tree that does not exist yet is skipped by name.
var readers = []string{"internal/config", "internal/toposcli", "test/conformance"}

// read matches a literal read of one variable, the only form the trees
// above read TOPOS_* variables in.
var read = regexp.MustCompile(`(?:[gG]etenv|LookupEnv)\("(TOPOS_[A-Z0-9_]+)"\)`)

// named matches one variable name as the table writes it.
var named = regexp.MustCompile("`(TOPOS_[A-Z0-9_]+)`")

// moduleRoot is the module root, found from this file rather than from
// the working directory.
func moduleRoot(t *testing.T) string {
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

// specTable returns the variables in the first column of spec 002's
// configuration table, and the ones whose Owner column is this spec.
func specTable(t *testing.T, root string) (all, owned []string) {
	t.Helper()
	var body []byte
	var err error
	for _, dir := range []string{"specs", filepath.Join("specs", ".archive")} {
		body, err = os.ReadFile(filepath.Join(root, dir, "002-scaffold-and-configuration.md"))
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("read spec 002: %v", err)
	}
	in := false
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "| Variable |") {
			in = true
			continue
		}
		if !in || strings.HasPrefix(line, "|---") {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(line, "|")
		if len(cells) < 5 {
			t.Fatalf("a row of the configuration table has too few cells: %q", line)
		}
		for _, m := range named.FindAllStringSubmatch(cells[1], -1) {
			all = append(all, m[1])
			if strings.TrimSpace(cells[4]) == "this spec" {
				owned = append(owned, m[1])
			}
		}
	}
	if len(all) == 0 {
		t.Fatal("spec 002 has no configuration table")
	}
	return all, owned
}

// readVariables returns every TOPOS_* variable the non-test files of the
// reader trees read, with the file that reads it.
func readVariables(t *testing.T, root string) map[string]string {
	t.Helper()
	found := map[string]string{}
	for _, tree := range readers {
		dir := filepath.Join(root, tree)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			for _, m := range read.FindAllStringSubmatch(string(b), -1) {
				found[m[1]] = filepath.ToSlash(rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return found
}

// TestConfigurationTableMatchesTheSpec holds spec 002's table as the one
// place an operator reads: every variable the server roles, the topos
// command and the conformance suite read is in it, and every variable
// the table gives to this spec is read. A variable another spec owns is
// proved read by that spec's own acceptance.
func TestConfigurationTableMatchesTheSpec(t *testing.T) {
	root := moduleRoot(t)
	all, owned := specTable(t, root)
	found := readVariables(t, root)
	if len(found) == 0 {
		t.Fatalf("no TOPOS_* read found under %s", strings.Join(readers, ", "))
	}
	for name, file := range found {
		if !slices.Contains(all, name) {
			t.Errorf("%s reads %s, which the configuration table of spec 002 does not list", file, name)
		}
	}
	if len(owned) == 0 {
		t.Fatal("the configuration table gives no variable to spec 002")
	}
	for _, name := range owned {
		if _, ok := found[name]; !ok {
			t.Errorf("the configuration table gives %s to spec 002, and nothing reads it", name)
		}
	}
}
