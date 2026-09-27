// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session_test

import (
	"testing"

	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/storetest"
)

func TestMemoryStoreConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) session.Store { return session.NewMemoryStore() })
}
