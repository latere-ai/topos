// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package gemini_test

import (
	"context"
	"testing"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/gemini"
)

// TestNotImplemented verifies that the Gemini stub compiles and returns the
// expected "not implemented" error. This keeps coverage honest per the repo
// rule that tests accompany every exported symbol.
func TestNotImplemented(t *testing.T) {
	a := gemini.New("", "")
	_, err := a.Stream(context.Background(), models.Request{
		Messages:  []models.Message{{Role: "user", Content: "hello"}},
		MaxTokens: 64,
	})
	if err == nil {
		t.Fatal("expected error from gemini stub, got nil")
	}
	if err.Error() != "gemini: not implemented" {
		t.Errorf("error = %q, want %q", err.Error(), "gemini: not implemented")
	}
}
