// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package machine

import "context"

const seesServers = true

// serverPorts reads /proc, which a Linux machine and the sandboxes of
// Cella mount for their own processes.
func serverPorts(_ context.Context, pgid int) ([]int, error) {
	return procServerPorts("/proc", pgid)
}
