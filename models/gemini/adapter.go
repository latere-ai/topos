// Package gemini will implement the [models.Model] interface against the
// Google Gemini GenerateContent API. The adapter body is a follow-up spec;
// this package exists to make the interface set visible and to ensure the
// build graph is complete.
package gemini

import (
	"context"
	"errors"

	"latere.ai/x/agents/internal/models"
)

// Adapter is a placeholder for the Gemini chat-completion adapter.
// Stream always returns an error; the full implementation is a follow-up spec.
type Adapter struct{}

// New returns a placeholder Adapter. The apiKey and baseURL parameters mirror
// the Anthropic adapter constructor for interface symmetry; they are unused
// until the body is implemented.
func New(apiKey, baseURL string) *Adapter {
	return &Adapter{}
}

// Stream implements [models.Model]. It always returns an error because the
// Gemini adapter body is a follow-up spec.
func (a *Adapter) Stream(_ context.Context, _ models.Request) (models.Stream, error) {
	return nil, errors.New("gemini: not implemented")
}

// Ensure Adapter implements models.Model at compile time.
var _ models.Model = (*Adapter)(nil)
