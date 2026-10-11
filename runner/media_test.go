// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import "testing"

// TestMediaTypeReadsTheNameThenTheBytes: an extension the standard table
// names decides the type whatever the bytes are, and a name with no
// extension, or one no table holds, is typed by its leading bytes (specs
// 044 and 064).
func TestMediaTypeReadsTheNameThenTheBytes(t *testing.T) {
	for _, c := range []struct {
		name, head, want string
	}{
		{"report.pdf", "not a pdf", "application/pdf"},
		{"chart.png", "GIF89a", "image/png"},
		{"page.html", "plain", "text/html; charset=utf-8"},
		{"papers", "PK\x03\x04rest", "application/zip"},
		{"report", "%PDF-1.7", "application/pdf"},
		{"data.topos-unknown", "\x00\x01\x02\x03", "application/octet-stream"},
		{"notes.topos-unknown", "plain words", "text/plain; charset=utf-8"},
		{"empty", "", "text/plain; charset=utf-8"},
	} {
		if got := MediaType(c.name, []byte(c.head)); got != c.want {
			t.Errorf("MediaType(%q, %q) = %q, want %q", c.name, c.head, got, c.want)
		}
	}
}
