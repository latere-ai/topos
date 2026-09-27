// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package dir

import (
	"errors"
	"fmt"
	"os"
)

// tryLock reports that this platform has no directory store lock yet;
// Windows takes LockFileEx (spec 004) when it gains one.
func tryLock(f *os.File) (bool, error) {
	return false, fmt.Errorf("dir: the single-writer lock: %w", errors.ErrUnsupported)
}

func unlock(f *os.File) error {
	return fmt.Errorf("dir: the single-writer lock: %w", errors.ErrUnsupported)
}
