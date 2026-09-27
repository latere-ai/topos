// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"testing"
	"time"

	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/internal/store/storetest"
)

func TestMemoryStoreConformance(t *testing.T) {
	storetest.Run(t, func(_ *testing.T, now func() time.Time) store.Store { return store.NewMemory(now) })
}

func TestCursorRoundTrip(t *testing.T) {
	if store.Cursor("") != "" {
		t.Fatal("an empty key has a cursor")
	}
	if k, err := store.Uncursor(store.Cursor("agent_x")); err != nil || k != "agent_x" {
		t.Fatalf("Uncursor = %q, %v", k, err)
	}
}
