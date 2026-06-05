package cella

import (
	"context"
	"errors"
	"testing"

	"latere.ai/x/agents/internal/sandbox"
)

func TestStaticTokenSource_Token(t *testing.T) {
	ts := NewStaticTokenSource("static-tok")
	got, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "static-tok" {
		t.Fatalf("got %q, want %q", got, "static-tok")
	}
}

func TestContextTokenSource_prefersContextBearer(t *testing.T) {
	// Even with a fallback present, a context bearer must win.
	ts := NewContextTokenSource(NewStaticTokenSource("fallback"))
	ctx := sandbox.WithBearer(context.Background(), "ctx-user-bearer")
	got, err := ts.Token(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ctx-user-bearer" {
		t.Fatalf("got %q, want context bearer %q", got, "ctx-user-bearer")
	}
}

func TestContextTokenSource_fallsBackWhenAbsent(t *testing.T) {
	ts := NewContextTokenSource(NewStaticTokenSource("fallback"))
	got, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "fallback" {
		t.Fatalf("got %q, want fallback %q", got, "fallback")
	}
}

func TestContextTokenSource_failsClosedWithoutFallback(t *testing.T) {
	// Production wiring: nil fallback. An absent bearer must error, never
	// silently downgrade to a service identity.
	ts := NewContextTokenSource(nil)
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected an error when no context bearer and no fallback, got nil")
	}
	if !errors.Is(err, errNoContextBearer) {
		t.Fatalf("got %v, want errNoContextBearer", err)
	}
}

func TestContextTokenSource_emptyContextBearerFailsClosed(t *testing.T) {
	// A context that stores an empty bearer must be treated as absent, not as
	// a valid empty credential.
	ts := NewContextTokenSource(nil)
	ctx := sandbox.WithBearer(context.Background(), "")
	_, err := ts.Token(ctx)
	if !errors.Is(err, errNoContextBearer) {
		t.Fatalf("got %v, want errNoContextBearer for an empty stored bearer", err)
	}
}
