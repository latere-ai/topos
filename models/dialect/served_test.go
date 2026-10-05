// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package dialect

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"latere.ai/x/pkg/llmdialect/bridge"
	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/test/stubs/luxstub"
)

func TestServedReadsTheDoorsModelList(t *testing.T) {
	stub := luxstub.New(t)
	stub.Models(
		bridge.Model{Name: "vendor/big", ContextWindow: 400_000, MaxOutputTokens: 128_000, InputModalities: []string{"text", "image"},
			Pricing: &bridge.ModelPricing{Currency: "USD", Input: "1.25", Output: "10", CachedInput: "0.125", CacheWrite: "1.5"}},
		bridge.Model{Name: "vendor/euro", ContextWindow: 1000, MaxOutputTokens: 100, Pricing: &bridge.ModelPricing{Currency: "EUR", Input: "1", Output: "2"}},
	)
	for _, c := range []struct {
		dialect ir.Dialect
		door    string
		header  string
	}{
		{ir.DialectAnthropicMessages, "/anthropic", "X-Api-Key"},
		{ir.DialectOpenAIResponses, "/openai", "Authorization"},
	} {
		e, err := Served(t.Context(), http.DefaultClient, models.Connection{BaseURL: stub.URL() + c.door + "/", Model: "vendor/big", Dialect: c.dialect, Credential: "k"})
		if err != nil {
			t.Fatal(err)
		}
		if e.Name != "vendor/big" || e.InputWindow != 400_000 || e.MaxOutputTokens != 128_000 || !e.Supports.Images || e.Pricing == nil ||
			*e.Pricing.Input != 1_250_000 || *e.Pricing.Output != 10_000_000 || *e.Pricing.CacheRead != 125_000 || *e.Pricing.CacheWrite != 1_500_000 {
			t.Fatalf("%s: %+v %+v", c.door, e, e.Pricing)
		}
		listed := stub.Listed()
		if h := listed[len(listed)-1].Get(c.header); h == "" {
			t.Fatalf("%s: the list was asked without the credential in %s", c.door, c.header)
		}
	}
	conn := models.Connection{BaseURL: stub.URL() + "/openai", Model: "vendor/euro", Dialect: ir.DialectOpenAIChat}
	if e, err := Served(t.Context(), http.DefaultClient, conn); err != nil || e.InputWindow != 1000 || e.Pricing != nil {
		t.Fatalf("prices in another currency: %+v, %v", e, err)
	}
	conn.Model = "vendor/absent"
	if e, err := Served(t.Context(), http.DefaultClient, conn); err != nil || e.Name != "" {
		t.Fatalf("a model the list does not name: %+v, %v", e, err)
	}
	if e, listed, err := Listed(t.Context(), http.DefaultClient, conn); err != nil || !listed || e.Name != "" {
		t.Fatalf("a list that does not name the model: %+v, listed %v, %v", e, listed, err)
	}
	conn.Model = "vendor/euro"
	if e, listed, err := Listed(t.Context(), http.DefaultClient, conn); err != nil || !listed || e.Name != "vendor/euro" {
		t.Fatalf("a list that names the model: %+v, listed %v, %v", e, listed, err)
	}
}

func TestServedRefusesWhatItCannotRead(t *testing.T) {
	answer := func(status int, body string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			// A failed write is the client gone; the test reads the
			// error from its side.
			if _, err := w.Write([]byte(body)); err != nil {
				return
			}
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	for name, base := range map[string]string{
		"not found":   answer(http.StatusNotFound, `{}`),
		"not a list":  answer(http.StatusOK, `<html>`),
		"another per": answer(http.StatusOK, `{"data":[{"id":"m","context_window":10,"pricing":{"per":1000,"input":"1"}}]}`),
	} {
		e, err := Served(t.Context(), http.DefaultClient, models.Connection{BaseURL: base, Model: "m"})
		if err != nil || e.Pricing != nil {
			t.Fatalf("%s: %+v, %v", name, e, err)
		}
	}
	for name, base := range map[string]string{"not found": answer(http.StatusNotFound, `{}`), "not a list": answer(http.StatusOK, `<html>`), "no model": answer(http.StatusOK, `{"data":[]}`)} {
		if _, listed, err := Listed(t.Context(), http.DefaultClient, models.Connection{BaseURL: base, Model: "m"}); err != nil || listed {
			t.Fatalf("%s: listed %v, %v", name, listed, err)
		}
	}
	if _, err := Served(t.Context(), http.DefaultClient, models.Connection{BaseURL: answer(http.StatusOK, `{"data":[{"id":"m","pricing":{"per":1000000,"input":"one"}}]}`), Model: "m"}); err == nil {
		t.Fatal("a price that is not a decimal was read")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Served(t.Context(), http.DefaultClient, models.Connection{BaseURL: "http://" + addr, Model: "m"}); err == nil {
		t.Fatal("a base that does not answer served figures")
	}
	if _, err := Served(t.Context(), http.DefaultClient, models.Connection{BaseURL: "http://[::1", Model: "m"}); err == nil {
		t.Fatal("an unparsable base served figures")
	}
}
