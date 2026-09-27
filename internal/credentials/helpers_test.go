// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package credentials

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// httptestServer answers every request with status and body, and returns
// its URL.
func httptestServer(t *testing.T, status int, body string) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		if _, err := io.WriteString(w, body); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(s.Close)
	return s.URL
}
