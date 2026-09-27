// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package host

import "testing"

// stagedSandbox skips the stage backend, which Windows has no host
// sandbox driver for.
func stagedSandbox(t *testing.T, _, _ string) *Sandbox {
	t.Skip("the host sandbox has no driver on windows")
	return nil
}
