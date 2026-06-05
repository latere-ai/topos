package sandbox

import (
	"context"
	"testing"
)

func TestBearerFromContext_absent(t *testing.T) {
	got, ok := BearerFromContext(context.Background())
	if ok {
		t.Fatalf("expected ok=false for a context with no bearer, got token %q", got)
	}
	if got != "" {
		t.Fatalf("expected empty token, got %q", got)
	}
}

func TestWithBearer_roundTrip(t *testing.T) {
	ctx := WithBearer(context.Background(), "tok-123")
	got, ok := BearerFromContext(ctx)
	if !ok {
		t.Fatal("expected ok=true after WithBearer")
	}
	if got != "tok-123" {
		t.Fatalf("got %q, want %q", got, "tok-123")
	}
}

func TestWithBearer_overrides(t *testing.T) {
	ctx := WithBearer(context.Background(), "first")
	ctx = WithBearer(ctx, "second")
	got, _ := BearerFromContext(ctx)
	if got != "second" {
		t.Fatalf("got %q, want %q (later WithBearer must win)", got, "second")
	}
}
