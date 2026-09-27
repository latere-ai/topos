// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/machine"
)

func TestFetch(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != fetchAccept || r.Header.Get("User-Agent") != fetchUserAgent || r.Method != http.MethodGet {
			http.Error(w, "bad request headers", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<p>hello</p>")
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	})
	mux.HandleFunc("/hop/{n}", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(r.PathValue("n"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if n == 0 {
			fmt.Fprint(w, "arrived")
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/hop/%d", n-1), http.StatusFound)
	})
	mux.HandleFunc("/ftp", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "ftp://example.com/file", http.StatusFound)
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", 100))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	res, err := f.h.Fetch(ctx, machine.FetchRequest{URL: srv.URL + "/page"})
	if err != nil || res.Status != 200 || string(res.Body) != "<p>hello</p>" || res.ContentType != "text/html; charset=utf-8" || res.URL != srv.URL+"/page" || res.Truncated {
		t.Fatalf("page %+v, %v", res, err)
	}
	res, err = f.h.Fetch(ctx, machine.FetchRequest{URL: srv.URL + "/missing"})
	if err != nil || res.Status != 404 || !strings.Contains(string(res.Body), "gone") {
		t.Fatalf("a missing page is a result: %+v, %v", res, err)
	}
	res, err = f.h.Fetch(ctx, machine.FetchRequest{URL: srv.URL + "/hop/5"})
	if err != nil || string(res.Body) != "arrived" || res.URL != srv.URL+"/hop/0" {
		t.Fatalf("five redirects %+v, %v", res, err)
	}
	if _, err := f.h.Fetch(ctx, machine.FetchRequest{URL: srv.URL + "/hop/6"}); err == nil || !strings.Contains(err.Error(), "stopped after 5 redirects") {
		t.Fatalf("six redirects: %v", err)
	}
	if _, err := f.h.Fetch(ctx, machine.FetchRequest{URL: srv.URL + "/ftp"}); err == nil || !strings.Contains(err.Error(), "is not an http or https URL") {
		t.Fatalf("a redirect to ftp: %v", err)
	}

	old := fetchMaxBody
	fetchMaxBody = 10
	res, err = f.h.Fetch(ctx, machine.FetchRequest{URL: srv.URL + "/big"})
	fetchMaxBody = old
	if err != nil || !res.Truncated || string(res.Body) != strings.Repeat("x", 10) {
		t.Fatalf("a long body %+v, %v", res, err)
	}
}

func TestFetchRefuses(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	for _, c := range []struct{ url, want string }{
		{"ftp://example.com/", "is not an http or https URL"},
		{"file:///etc/passwd", "is not an http or https URL"},
		{"https:///path", "names no host"},
		{"http://%zz", "the URL"},
		{"http://127.0.0.1:1/", "machine: fetch:"},
	} {
		if _, err := f.h.Fetch(ctx, machine.FetchRequest{URL: c.url}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v, want %q", c.url, err, c.want)
		}
	}
	if err := f.h.Release(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.Fetch(ctx, machine.FetchRequest{URL: "https://example.com/"}); !errors.Is(err, machine.ErrReleased) {
		t.Fatalf("a fetch after release: %v", err)
	}
}

func TestFetchTimeout(t *testing.T) {
	f := open(t)
	stall := func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	body := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		fmt.Fprint(w, "partial")
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-r.Context().Done()
	}
	old := fetchTimeout
	fetchTimeout = 200 * time.Millisecond
	t.Cleanup(func() { fetchTimeout = old })
	for name, h := range map[string]http.HandlerFunc{"headers": stall, "body": body} {
		srv := httptest.NewServer(h)
		start := time.Now()
		_, err := f.h.Fetch(t.Context(), machine.FetchRequest{URL: srv.URL})
		srv.Close()
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 10*time.Second {
			t.Fatalf("%s: %v after %s", name, err, time.Since(start))
		}
	}
	// A caller that cancels is not a timeout.
	srv := httptest.NewServer(http.HandlerFunc(stall))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := f.h.Fetch(ctx, machine.FetchRequest{URL: srv.URL})
	if err == nil || errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled fetch: %v", err)
	}
}
