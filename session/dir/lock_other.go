// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build !unix && !windows

package dir

import (
	"errors"
	"fmt"
	"os"
)

// tryLock reports that this platform has no directory store lock: Unix
// takes flock and Windows LockFileEx (spec 004), and a platform with
// neither refuses to take one.
func tryLock(f *os.File) (bool, error) {
	return false, fmt.Errorf("dir: the single-writer lock: %w", errors.ErrUnsupported)
}

func unlock(f *os.File) error {
	return fmt.Errorf("dir: the single-writer lock: %w", errors.ErrUnsupported)
}
