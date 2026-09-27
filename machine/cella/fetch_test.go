// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/machine"
)

// needCurl skips a fetch test where the sandbox, the machine the tests
// run on, has no curl.
func needCurl(t *testing.T) {
	t.Helper()
	for _, p := range []string{"/usr/bin/curl", "/bin/curl", "/usr/local/bin/curl"} {
		if _, err := os.Stat(p); err == nil {
			return
		}
	}
	t.Skip("no curl on this machine, which the stub's sandboxes run commands on")
}

func fetchServer(t *testing.T) *httptest.Server {
	t.Helper()
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
	mux.HandleFunc("/exact", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("y", 10))
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchRunsInTheSandbox(t *testing.T) {
	needCurl(t)
	f := open(t)
	srv := fetchServer(t)
	ctx := t.Context()
	res, err := f.m.Fetch(ctx, machine.FetchRequest{URL: srv.URL + "/page"})
	if err != nil || res.Status != 200 || string(res.Body) != "<p>hello</p>" || res.ContentType != "text/html; charset=utf-8" || res.URL != srv.URL+"/page" || res.Truncated {
		t.Fatalf("page %+v, %v", res, err)
	}
	// The fetch is a command in the sandbox, not a request of the runner.
	sessions := 0
	for _, r := range f.stub.Requests() {
		if strings.HasSuffix(r.Path, "/exec") {
			sessions++
		}
	}
	if sessions == 0 {
		t.Error("the fetch ran no command in the sandbox")
	}
	res, err = f.m.Fetch(ctx, machine.FetchRequest{URL: srv.URL + "/missing"})
	if err != nil || res.Status != 404 {
		t.Errorf("a 404 is a result: %+v, %v", res, err)
	}
	res, err = f.m.Fetch(ctx, machine.FetchRequest{URL: srv.URL + "/hop/5"})
	if err != nil || string(res.Body) != "arrived" || res.URL != srv.URL+"/hop/0" {
		t.Errorf("five redirects: %+v, %v", res, err)
	}
	if _, err := f.m.Fetch(ctx, machine.FetchRequest{URL: srv.URL + "/hop/6"}); err == nil || !strings.Contains(err.Error(), "curl exited 47") {
		t.Errorf("six redirects: %v", err)
	}
	if _, err := f.m.Fetch(ctx, machine.FetchRequest{URL: srv.URL + "/ftp"}); err == nil {
		t.Error("a redirect to ftp was followed")
	}
	old := fetchMaxBody
	fetchMaxBody = 10
	t.Cleanup(func() { fetchMaxBody = old })
	res, err = f.m.Fetch(ctx, machine.FetchRequest{URL: srv.URL + "/big"})
	if err != nil || len(res.Body) != 10 || !res.Truncated {
		t.Errorf("a long body: %d bytes, truncated %v, %v", len(res.Body), res.Truncated, err)
	}
	res, err = f.m.Fetch(ctx, machine.FetchRequest{URL: srv.URL + "/exact"})
	if err != nil || len(res.Body) != 10 || res.Truncated {
		t.Errorf("a body at the limit: %d bytes, truncated %v, %v", len(res.Body), res.Truncated, err)
	}
}

func TestFetchRefuses(t *testing.T) {
	f := open(t)
	for _, u := range []string{"file:///etc/passwd", "ftp://example.com/x", "http://", "::bad"} {
		if _, err := f.m.Fetch(t.Context(), machine.FetchRequest{URL: u}); err == nil {
			t.Errorf("%s was fetched", u)
		}
	}
}

func TestFetchTimeout(t *testing.T) {
	needCurl(t)
	f := open(t)
	srv := fetchServer(t)
	old := fetchTimeout
	fetchTimeout = 300 * time.Millisecond
	t.Cleanup(func() { fetchTimeout = old })
	if _, err := f.m.Fetch(t.Context(), machine.FetchRequest{URL: srv.URL + "/slow"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a slow page: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(300*time.Millisecond, cancel)
	fetchTimeout = 10 * time.Second
	if _, err := f.m.Fetch(ctx, machine.FetchRequest{URL: srv.URL + "/slow"}); !errors.Is(err, context.Canceled) {
		t.Errorf("a canceled fetch: %v", err)
	}
}

func TestParseFetch(t *testing.T) {
	for name, out := range map[string]string{
		"no answer":      "",
		"a short head":   "ok\n200\n",
		"a bad status":   "ok\nabc\ntext/plain\nhttp://x\nbody",
		"an error":       "error 6 curl: (6) Could not resolve host: nowhere",
		"a missing curl": "error 127 curl is not installed in the sandbox\n",
		"a stray answer": "something else",
	} {
		if _, err := parseFetch([]byte(out)); err == nil {
			t.Errorf("%s was read as a result", name)
		}
	}
	if _, err := parseFetch([]byte("error 28 curl: (28) Operation timed out")); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("curl's timeout: %v", err)
	}
	res, err := parseFetch([]byte("ok\n200\n\nhttp://x/\nbody\nlines"))
	if err != nil || res.Status != 200 || res.ContentType != "" || res.URL != "http://x/" || string(res.Body) != "body\nlines" {
		t.Errorf("parse = %+v %v", res, err)
	}
}
