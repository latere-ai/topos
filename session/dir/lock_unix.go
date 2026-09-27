// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package dir

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// tryLock takes the exclusive lock on f without waiting. It reports
// false when another open file holds it.
func tryLock(f *os.File) (bool, error) {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return false, nil
		default:
			return false, fmt.Errorf("dir: lock: %w", err)
		}
	}
}

// unlock releases the lock on f.
func unlock(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("dir: unlock: %w", err)
	}
	return nil
}
