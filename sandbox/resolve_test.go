// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package sandbox_test

import (
	"os"
	"path/filepath"
	"testing"

	"latere.ai/x/topos/sandbox"
)

func TestResolveRootPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "real"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		"alias": ".env", "directory": "real", "chain": "directory/../alias",
		"loop": "loop", "escape": "../outside", "absolute": filepath.Join(dir, "real"),
	} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	for name, want := range map[string]string{
		".": ".", "real/..": ".", "alias": ".env", "chain": ".env",
		"directory/new/file": "real/new/file", "missing/file": "missing/file",
	} {
		if got, err := sandbox.ResolveRootPath(root, name); err != nil || filepath.ToSlash(got) != want {
			t.Errorf("resolve %q = %q, %v; want %q", name, got, err, want)
		}
	}
	for _, name := range []string{"", "../outside", "escape", "absolute", "loop", ".env/child", ".env/../public"} {
		if got, err := sandbox.ResolveRootPath(root, name); err == nil {
			t.Errorf("resolve invalid %q = %q", name, got)
		}
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := sandbox.ResolveRootPath(root, "file"); err == nil {
		t.Fatal("closed directory accepted")
	}
}
