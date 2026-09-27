// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package dir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ServeLock is the file one serving toposd holds under its data
// directory.
const ServeLock = "serve.lock"

// LockServe takes the lock a serving toposd holds on its data directory
// for as long as it runs: the directory store is a one-replica mode
// (spec 014). A second serve on the same directory is refused with the
// pid of the one that holds it. release lets the lock go.
func LockServe(dataDir string) (release func() error, err error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("dir: create %s: %w", dataDir, err)
	}
	path := filepath.Join(dataDir, ServeLock)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("dir: open %s: %w", path, err)
	}
	ok, err := tryLock(f)
	if err != nil || !ok {
		b, rerr := os.ReadFile(path)
		cerr := f.Close()
		if err != nil {
			return nil, errors.Join(err, cerr)
		}
		pid := strings.TrimSpace(string(b))
		if rerr != nil || pid == "" {
			pid = "unknown"
		}
		return nil, errors.Join(fmt.Errorf("data directory in use by pid %s", pid), cerr)
	}
	if err := f.Truncate(0); err != nil {
		return nil, errors.Join(err, unlock(f), f.Close())
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		return nil, errors.Join(err, unlock(f), f.Close())
	}
	return func() error { return errors.Join(unlock(f), f.Close()) }, nil
}
