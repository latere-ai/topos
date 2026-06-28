// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package cella

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/topos/sandbox"
)

// callMapError builds a response, runs mapError, and closes the body — mirroring
// doJSON, which owns closing.
func callMapError(status int, body string) error {
	r := &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
	defer r.Body.Close() //nolint:errcheck
	return mapError(r)
}

func TestMapError(t *testing.T) {
	t.Run("404 maps to ErrNotFound", func(t *testing.T) {
		err := callMapError(http.StatusNotFound, `{"code":"not_found","message":"sandbox not found","request_id":"req_1"}`)
		if !errors.Is(err, sandbox.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("409 maps to ErrConflict", func(t *testing.T) {
		err := callMapError(http.StatusConflict, `{"code":"conflict","message":"name taken"}`)
		if !errors.Is(err, sandbox.ErrConflict) {
			t.Fatalf("got %v, want ErrConflict", err)
		}
	})

	t.Run("500 maps to *APIError with all fields", func(t *testing.T) {
		err := callMapError(http.StatusInternalServerError, `{"code":"internal","message":"boom","request_id":"req_xyz"}`)
		var apiErr *sandbox.APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("got %T (%v), want *sandbox.APIError", err, err)
		}
		if apiErr.Status != 500 || apiErr.Code != "internal" || apiErr.Message != "boom" || apiErr.RequestID != "req_xyz" {
			t.Fatalf("APIError = %+v, want 500/internal/boom/req_xyz", apiErr)
		}
	})

	t.Run("429 with non-JSON body still yields *APIError", func(t *testing.T) {
		err := callMapError(http.StatusTooManyRequests, "slow down")
		var apiErr *sandbox.APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("got %T, want *sandbox.APIError", err)
		}
		if apiErr.Status != 429 {
			t.Fatalf("status = %d, want 429", apiErr.Status)
		}
	})
}

func TestStaticTokenSource(t *testing.T) {
	tok, err := StaticTokenSource("abc").Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "abc" {
		t.Fatalf("token = %q, want abc", tok)
	}
	if _, err := StaticTokenSource("").Token(context.Background()); err == nil {
		t.Fatal("empty static token: want error, got nil")
	}
}

func TestContextTokenSource(t *testing.T) {
	ctx := sandbox.WithBearer(context.Background(), "user-bearer")
	tok, err := ContextTokenSource{}.Token(ctx)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "user-bearer" {
		t.Fatalf("token = %q, want user-bearer", tok)
	}

	if _, err := (ContextTokenSource{}).Token(context.Background()); err == nil {
		t.Fatal("missing bearer: want error, got nil")
	}
}

func TestNewPanicsOnMissingConfig(t *testing.T) {
	assertPanic(t, "missing BaseURL", func() { New(Options{Token: StaticTokenSource("x")}) })
	assertPanic(t, "missing Token", func() { New(Options{BaseURL: "http://x"}) })
}

func assertPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s: expected panic, got none", name)
		}
	}()
	fn()
}
