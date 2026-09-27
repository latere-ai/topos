// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package dir

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"latere.ai/x/topos/session"
)

// TestTheLockIsExclusiveAcrossOpenFiles is the contract every platform's
// tryLock and unlock keep: a second open file of a held lock is refused
// without waiting and reads the holder record the first wrote, and takes
// the lock once the first releases it. A platform with no lock refuses
// with ErrUnsupported and skips.
func TestTheLockIsExclusiveAcrossOpenFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	openLock := func() *os.File {
		t.Helper()
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := f.Close(); err != nil {
				t.Error(err)
			}
		})
		return f
	}
	first, second := openLock(), openLock()

	ok, err := tryLock(first)
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skip("this platform has no directory store lock")
	}
	if err != nil || !ok {
		t.Fatalf("the first lock: %v, %v", ok, err)
	}
	record, err := session.Marshal(session.Holder{Runner: "run_first", PID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.WriteAt(record, 0); err != nil {
		t.Fatal(err)
	}
	if ok, err := tryLock(second); err != nil || ok {
		t.Fatalf("a second open file took a held lock: %v, %v", ok, err)
	}
	if h := readHolder(second); h.Runner != "run_first" || h.PID != 7 {
		t.Fatalf("the holder read through the second open file: %+v", h)
	}
	if err := unlock(first); err != nil {
		t.Fatal(err)
	}
	if ok, err := tryLock(second); err != nil || !ok {
		t.Fatalf("the second open file after the release: %v, %v", ok, err)
	}
	if ok, err := tryLock(first); err != nil || ok {
		t.Fatalf("the first open file took the lock the second holds: %v, %v", ok, err)
	}
	if err := unlock(second); err != nil {
		t.Fatal(err)
	}
}

// TestTheStoreBuildsForWindows holds the session tree, the directory
// store with its LockFileEx lock and their tests, to compiling and
// passing go vet for Windows, so a host without a Windows runner proves
// the Windows code at least type-checks against the calls it makes.
func TestTheStoreBuildsForWindows(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles the session tree")
	}
	cmd := exec.CommandContext(t.Context(), "go", "vet", "latere.ai/x/topos/session/...")
	cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go vet for windows: %v\n%s", err, out)
	}
}
