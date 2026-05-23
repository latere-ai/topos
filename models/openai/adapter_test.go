package openai_test

import (
	"context"
	"testing"

	"latere.ai/x/agents/internal/models"
	"latere.ai/x/agents/internal/models/openai"
)

// TestNotImplemented verifies that the OpenAI stub compiles and returns the
// expected "not implemented" error. This keeps coverage honest per the repo
// rule that tests accompany every exported symbol.
func TestNotImplemented(t *testing.T) {
	a := openai.New("", "")
	_, err := a.Stream(context.Background(), models.Request{
		Messages:  []models.Message{{Role: "user", Content: "hello"}},
		MaxTokens: 64,
	})
	if err == nil {
		t.Fatal("expected error from openai stub, got nil")
	}
	if err.Error() != "openai: not implemented" {
		t.Errorf("error = %q, want %q", err.Error(), "openai: not implemented")
	}
}
