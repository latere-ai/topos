// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package dir

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// lockOffset is where the one locked byte sits, far past the holder
// record at the start of the lock file. A Windows byte-range lock is
// mandatory for the bytes it covers, so a lock over the record would stop
// another process from reading who holds the session.
const lockOffset uint64 = 1 << 62

// lockRange is the locked byte's position, as LockFileEx and UnlockFileEx
// take it.
func lockRange() *windows.Overlapped {
	return &windows.Overlapped{Offset: uint32(lockOffset & 0xffffffff), OffsetHigh: uint32(lockOffset >> 32)}
}

// tryLock takes the exclusive lock on f without waiting, with LockFileEx.
// It reports false when another open file holds it. Windows releases the
// lock when the handle closes or the process exits.
func tryLock(f *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, lockRange())
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION):
		return false, nil
	default:
		return false, fmt.Errorf("dir: lock: %w", err)
	}
}

// unlock releases the lock on f.
func unlock(f *os.File) error {
	if err := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, lockRange()); err != nil {
		return fmt.Errorf("dir: unlock: %w", err)
	}
	return nil
}
