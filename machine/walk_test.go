// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestAWalkListsTheWorkingDirectory: a walk lists every regular file in
// walk order with its size, its permission bits and its digest; it leaves
// out a directory LeftOut names at any depth and a symbolic link; it
// hashes no file past MaxFileBytes and none past MaxFiles or MaxBytes,
// naming why; and it stops at MaxListed and says so (spec 073).
func TestAWalkListsTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, data string, mode os.FileMode) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	write("a-b.txt", "ab", 0o644)
	write("a/x.txt", "x", 0o644)
	write("a/node_modules/dep.js", "dep", 0o644)
	write("node_modules/top.js", "top", 0o644)
	write("sub/.git/HEAD", "ref", 0o644)
	write("run.sh", "#!/bin/sh\n", 0o755)
	write("big.bin", "0123456789A", 0o644)
	if err := os.Symlink("a/x.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	q := WalkRequest{LeftOut: []string{"node_modules", ".git"}, MaxFiles: 10, MaxBytes: 1 << 20, MaxFileBytes: 10, MaxListed: 100}
	got, err := WalkFS(t.Context(), os.DirFS(dir), q)
	if err != nil {
		t.Fatal(err)
	}
	sum := func(s string) string {
		h := sha256.Sum256([]byte(s))
		return hex.EncodeToString(h[:])
	}
	want := []WalkedFile{
		{Path: "a/x.txt", Size: 1, Mode: 0o644, SHA256: sum("x")},
		{Path: "a-b.txt", Size: 2, Mode: 0o644, SHA256: sum("ab")},
		{Path: "big.bin", Size: 11, Mode: 0o644, Reason: WalkTooLarge},
		{Path: "run.sh", Size: 10, Mode: 0o755, SHA256: sum("#!/bin/sh\n")},
	}
	if got.Cut || !slices.Equal(got.Files, want) {
		t.Fatalf("the walk\n%+v\nwant\n%+v", got, want)
	}
	if !slices.IsSortedFunc(got.Files, func(a, b WalkedFile) int { return WalkOrder(a.Path, b.Path) }) {
		t.Fatal("the walk is not in walk order")
	}

	q.MaxFiles = 1
	got, err = WalkFS(t.Context(), os.DirFS(dir), q)
	if err != nil || got.Files[0].SHA256 == "" || got.Files[1].Reason != WalkLimit || got.Files[3].Reason != WalkLimit {
		t.Fatalf("a walk past its files: %+v, %v", got, err)
	}
	q.MaxFiles, q.MaxBytes = 10, 2
	got, err = WalkFS(t.Context(), os.DirFS(dir), q)
	if err != nil || got.Files[0].SHA256 == "" || got.Files[1].Reason != WalkLimit {
		t.Fatalf("a walk past its bytes: %+v, %v", got, err)
	}
	q.MaxListed = 2
	got, err = WalkFS(t.Context(), os.DirFS(dir), q)
	if err != nil || !got.Cut || len(got.Files) != 2 {
		t.Fatalf("a walk past what it lists: %+v, %v", got, err)
	}
	if _, err := WalkFS(t.Context(), os.DirFS(filepath.Join(dir, "none")), q); err == nil {
		t.Fatal("a walk of nothing succeeded")
	}
	ctx, cancel := contextCanceled(t)
	defer cancel()
	if _, err := WalkFS(ctx, os.DirFS(dir), q); err == nil {
		t.Fatal("a canceled walk succeeded")
	}
	for _, c := range []struct {
		a, b string
		want int
	}{{"a/x", "a-b", -1}, {"a/x", "a.txt", -1}, {"b", "a/z", 1}, {"a", "a", 0}} {
		if got := WalkOrder(c.a, c.b); got != c.want {
			t.Errorf("WalkOrder(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// contextCanceled is a context that is canceled already.
func contextCanceled(t *testing.T) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx, cancel
}
