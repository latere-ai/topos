// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package dialect

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"latere.ai/x/topos/test/stubs/luxstub"
)

// TestDiscoverFindsLuxsDoors: a Lux root names its doors; a URL that
// already names a door is not asked; a provider's base, which answers
// the path with another status or another document, has none; a base
// that does not answer is an error.
func TestDiscoverFindsLuxsDoors(t *testing.T) {
	ctx := t.Context()
	lux := luxstub.New(t)
	doors, err := Discover(ctx, http.DefaultClient, lux.URL()+"/")
	if err != nil || doors["anthropic"] != lux.URL()+"/anthropic" || doors["openai"] != lux.URL()+"/openai" {
		t.Fatalf("a Lux root: %v, %v", doors, err)
	}
	var asked atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		switch r.URL.Path {
		case "/other/.well-known/lux":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"not lux","doors":{"anthropic":"x"}}`))
		case "/html/.well-known/lux":
			_, _ = w.Write([]byte("<html></html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(provider.Close)
	for _, base := range []string{provider.URL, provider.URL + "/other", provider.URL + "/html"} {
		if doors, err := Discover(ctx, provider.Client(), base); err != nil || doors != nil {
			t.Errorf("%s: %v, %v", base, doors, err)
		}
	}
	before := asked.Load()
	if doors, err := Discover(ctx, provider.Client(), provider.URL+"/anthropic"); err != nil || doors != nil || asked.Load() != before {
		t.Errorf("a door URL: %v, %v, asked %d times", doors, err, asked.Load()-before)
	}
	if doors, err := Discover(ctx, http.DefaultClient, "scripted:/tmp/s.yaml"); err != nil || doors != nil {
		t.Errorf("a scripted connection: %v, %v", doors, err)
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	if _, err := Discover(ctx, http.DefaultClient, gone.URL); err == nil {
		t.Error("a base that does not answer had no error")
	}
}
