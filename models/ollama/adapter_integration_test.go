// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

//go:build integration

package ollama_test

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/latere-ai/topos/models"
	"github.com/latere-ai/topos/models/ollama"
)

// TestIntegrationRealOllama performs a live round-trip against a running Ollama
// server. It is gated on the OLLAMA_HOST environment variable; the test is
// skipped when the variable is unset.
//
// Usage:
//
//	OLLAMA_HOST=http://localhost:11434 OLLAMA_MODEL=llama3.1 \
//	  go test -tags integration ./internal/models/ollama/...
func TestIntegrationRealOllama(t *testing.T) {
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		t.Skip("OLLAMA_HOST not set; skipping Ollama integration test")
	}
	modelName := os.Getenv("OLLAMA_MODEL")
	if modelName == "" {
		modelName = "llama3.1"
	}

	a := ollama.New(host, modelName)
	stream, err := a.Stream(context.Background(), models.Request{
		System:    "You are a helpful assistant. Keep responses very brief.",
		Messages:  []models.Message{{Role: "user", Content: "Say 'hello' in one word."}},
		MaxTokens: 32,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var sawText, sawDone bool
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
			if ev.TextDelta != "" {
				sawText = true
			}
		case models.KindDone:
			sawDone = true
		}
	}

	if !sawText {
		t.Error("expected at least one non-empty text delta")
	}
	if !sawDone {
		t.Error("expected a KindDone event")
	}
}
