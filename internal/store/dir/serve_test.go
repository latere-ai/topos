// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package dir

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestASecondServeIsRefusedWithThePidOfTheFirst(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	release, err := LockServe(root)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, ServeLock))
	if err != nil || strings.TrimSpace(string(b)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("the lock holds %q, %v", b, err)
	}
	if _, err := LockServe(root); err == nil || !strings.Contains(err.Error(), "data directory in use by pid "+strconv.Itoa(os.Getpid())) {
		t.Fatalf("a second serve: %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	again, err := LockServe(root)
	if err != nil {
		t.Fatalf("after a release: %v", err)
	}
	if err := again(); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LockServe(filepath.Join(blocked, "data")); err == nil {
		t.Fatal("locked a directory under a file")
	}
}
