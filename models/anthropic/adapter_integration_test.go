// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

//go:build integration

package anthropic_test

import (
	"context"
	"io"
	"os"
	"testing"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/anthropic"
)

// TestIntegrationStream hits the real Anthropic API. It is gated on the
// ANTHROPIC_API_KEY environment variable and is skipped when unset.
//
// Run with:
//
//	ANTHROPIC_API_KEY=sk-... go test -tags integration ./internal/models/anthropic/
func TestIntegrationStream(t *testing.T) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping integration test")
	}

	a := anthropic.New(apiKey, "")
	stream, err := a.Stream(context.Background(), models.Request{
		System:    "You are a helpful assistant. Answer very briefly.",
		Messages:  []models.Message{{Role: "user", Content: "Say the word PONG and nothing else."}},
		MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var text string
	var gotDone bool
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		switch ev.Kind {
		case models.KindTextDelta:
			text += ev.TextDelta
		case models.KindDone:
			gotDone = true
		case models.KindUsage:
			if ev.Usage.InputTokens == 0 && ev.Usage.OutputTokens == 0 {
				t.Logf("Usage event with zero tokens (may be partial): %+v", ev.Usage)
			}
		}
	}

	if !gotDone {
		t.Error("no KindDone event received")
	}
	if text == "" {
		t.Error("no text received from model")
	}
	t.Logf("model response: %q", text)
}
