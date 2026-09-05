// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveRootPath expands relative symlinks using an open directory handle.
// It returns a root-relative path suitable for policy checks and Root methods.
// Missing components are preserved for file creation. Absolute links, escapes,
// and cycles are rejected. The eventual I/O must still use Root methods: this
// name resolution alone does not protect against concurrent filesystem changes.
func ResolveRootPath(root *os.Root, name string) (string, error) {
	if !filepath.IsLocal(name) {
		return "", ErrConfined
	}
	pending := strings.Split(filepath.ToSlash(name), "/")
	var resolved []string
	links := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return "", ErrConfined
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		candidate := filepath.Join(append(resolved, part)...)
		info, err := root.Lstat(candidate)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			links++
			if links > 40 {
				return "", fmt.Errorf("%w: too many symbolic links", ErrConfined)
			}
			target, err := root.Readlink(candidate)
			if err != nil {
				return "", err
			}
			if filepath.IsAbs(target) || filepath.VolumeName(target) != "" {
				return "", ErrConfined
			}
			pending = append(strings.Split(filepath.ToSlash(target), "/"), pending...)
			continue
		}
		if err == nil && !info.IsDir() && len(pending) > 0 {
			return "", fmt.Errorf("resolve %q: %q is not a directory", name, candidate)
		}
		resolved = append(resolved, part)
	}
	if len(resolved) == 0 {
		return ".", nil
	}
	return filepath.Join(resolved...), nil
}
