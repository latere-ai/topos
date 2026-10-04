// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin

package machine

import "context"

const seesServers = false

// serverPorts answers ErrServersUnseen: this system's sockets are not
// read, so its commands keep their timeouts alone.
func serverPorts(context.Context, int) ([]int, error) { return nil, ErrServersUnseen }
