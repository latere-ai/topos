// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package dir

import "syscall"

// errNotEmpty is what renaming a directory over a non-empty one returns.
var errNotEmpty error = syscall.ENOTEMPTY
