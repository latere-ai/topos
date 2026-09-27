// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package storetest_test

import (
	"testing"

	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/storetest"
)

func TestSuiteOnTheMemoryStore(t *testing.T) {
	storetest.Run(t, func(t *testing.T) session.Store { return session.NewMemoryStore() })
}
