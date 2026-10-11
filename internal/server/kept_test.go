// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/session"
)

// putBlob stores body as a blob of the session id.
func (f *fixture) putBlob(id string, body []byte) session.Digest {
	f.t.Helper()
	d, err := f.sessions.PutBlob(f.t.Context(), id, bytes.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

// TestABlobAnswersByItsBytes: the blob route answers a blob whose bytes
// are PNG, JPEG, GIF or WebP as that image type, inline, and any other
// blob, an SVG and a file named like an image included, as an
// application/octet-stream attachment; both with nosniff, the sandboxing
// policy, the immutable private cache and the digest as ETag (spec 055).
func TestABlobAnswersByItsBytes(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	for _, c := range []struct {
		name, body, media, disposition string
	}{
		{"png", "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR", "image/png", "inline"},
		{"jpeg", "\xff\xd8\xff\xe0\x00\x10JFIF", "image/jpeg", "inline"},
		{"gif", "GIF89a\x01\x00\x01\x00", "image/gif", "inline"},
		{"webp", "RIFF\x24\x00\x00\x00WEBPVP8 ", "image/webp", "inline"},
		{"svg", `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`, "application/octet-stream", "attachment"},
		{"html", "<!doctype html><title>x</title>", "application/octet-stream", "attachment"},
		{"short", "\x89P", "application/octet-stream", "attachment"},
		{"empty", "", "application/octet-stream", "attachment"},
	} {
		d := f.putBlob(s.ID, []byte(c.body))
		a := f.do(http.MethodGet, "/v1/sessions/"+s.ID+"/blobs/"+string(d), "alice", "")
		if a.status != http.StatusOK || string(a.body) != c.body {
			t.Fatalf("%s: %d, %q", c.name, a.status, a.body)
		}
		for h, want := range map[string]string{
			"Content-Type":            c.media,
			"Content-Disposition":     c.disposition,
			"X-Content-Type-Options":  "nosniff",
			"Content-Security-Policy": "sandbox; default-src 'none'",
			"Cache-Control":           "private, max-age=31536000, immutable",
			"ETag":                    strconv.Quote(string(d)),
		} {
			if got := a.header.Get(h); got != want {
				t.Errorf("%s: %s is %q, want %q", c.name, h, got, want)
			}
		}
	}
	if a := f.do(http.MethodGet, "/v1/sessions/"+s.ID+"/blobs/"+string(f.putBlob(s.ID, []byte("\x89PNG\r\n\x1a\n"))), "bob", ""); a.status != http.StatusNotFound {
		t.Fatalf("a caller who may not read the session: %d", a.status)
	}
}

// TestRedactingAnAnswerTakesItsImages: redacting a files.kept alone
// removes its image and leaves its message; redacting an agent.message
// redacts its files.kept in the same append and removes its image, and
// the blob route then answers not_found for both (spec 055).
func TestRedactingAnAnswerTakesItsImages(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	answerWith := func(name string, d session.Digest) (session.Event, session.Event) {
		t.Helper()
		msg := ev(t, session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockText, Text: "![A chart](" + name + ")"}}}, StopReason: ir.StopEndTurn})
		kept := ev(t, session.TypeFilesKept, session.FilesKept{Message: msg.ID,
			Files:   []session.KeptFile{{Path: name, Resolved: "/work/" + name, Blob: d, MediaType: "image/png", Size: 9, Width: 1, Height: 1}},
			Skipped: []session.SkippedFile{}})
		f.put(s.ID, msg, kept)
		return msg, kept
	}
	read := func(d session.Digest) int {
		return f.do(http.MethodGet, "/v1/sessions/"+s.ID+"/blobs/"+string(d), "alice", "").status
	}
	redact := func(id string) {
		t.Helper()
		if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events/"+id+"/redact", "alice", `{"reason":"the wrong chart"}`); a.status != http.StatusNoContent {
			t.Fatalf("redact %s: %d %s", id, a.status, a.body)
		}
	}
	redacted := func(id string) bool {
		for _, e := range f.log(s.ID) {
			if e.ID == id {
				return e.Redacted()
			}
		}
		t.Fatalf("no event %s", id)
		return false
	}

	first := f.putBlob(s.ID, []byte("\x89PNG\r\n\x1a\n1"))
	msg, kept := answerWith("first.png", first)
	if read(first) != http.StatusOK {
		t.Fatal("the kept image is not served")
	}
	redact(kept.ID)
	if !redacted(kept.ID) || redacted(msg.ID) || read(first) != http.StatusNotFound {
		t.Fatalf("redacting files.kept alone: kept %t, message %t, image %d", redacted(kept.ID), redacted(msg.ID), read(first))
	}

	second := f.putBlob(s.ID, []byte("\x89PNG\r\n\x1a\n2"))
	msg, kept = answerWith("second.png", second)
	before := len(f.log(s.ID))
	redact(msg.ID)
	if !redacted(msg.ID) || !redacted(kept.ID) || read(second) != http.StatusNotFound {
		t.Fatalf("redacting the answer: message %t, kept %t, image %d", redacted(msg.ID), redacted(kept.ID), read(second))
	}
	if after := f.log(s.ID); len(after) != before+2 || after[before].Type != session.TypeEventRedacted || after[before+1].Type != session.TypeEventRedacted {
		t.Fatalf("redacting the answer appended %d events", len(after)-before)
	}
}

// TestAKeptFileIsADownloadRedactedWithItsAnswer: a file an answer linked
// that is no image, a PDF, an archive and an HTML page, is answered by
// the blob route as an application/octet-stream attachment with nosniff
// and the sandboxing policy, never inline and never as a document;
// redacting its files.kept alone removes it and leaves the answer, and
// redacting the answer redacts its files.kept in the same append and
// removes the file (spec 064).
func TestAKeptFileIsADownloadRedactedWithItsAnswer(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	answerWith := func(name, media, body string) (session.Event, session.Event, session.Digest) {
		t.Helper()
		d := f.putBlob(s.ID, []byte(body))
		msg := ev(t, session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockText, Text: "[Download](" + name + ")"}}}, StopReason: ir.StopEndTurn})
		kept := ev(t, session.TypeFilesKept, session.FilesKept{Message: msg.ID,
			Files:   []session.KeptFile{{Path: name, Resolved: "/work/" + name, Blob: d, MediaType: media, Size: int64(len(body))}},
			Skipped: []session.SkippedFile{}})
		f.put(s.ID, msg, kept)
		return msg, kept, d
	}
	read := func(d session.Digest) answer {
		return f.do(http.MethodGet, "/v1/sessions/"+s.ID+"/blobs/"+string(d), "alice", "")
	}
	redact := func(id string) {
		t.Helper()
		if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events/"+id+"/redact", "alice", `{"reason":"the wrong file"}`); a.status != http.StatusNoContent {
			t.Fatalf("redact %s: %d %s", id, a.status, a.body)
		}
	}
	redacted := func(id string) bool {
		for _, e := range f.log(s.ID) {
			if e.ID == id {
				return e.Redacted()
			}
		}
		t.Fatalf("no event %s", id)
		return false
	}
	for _, c := range []struct{ name, media, body string }{
		{"report.pdf", "application/pdf", "%PDF-1.7 report"},
		{"papers.zip", "application/zip", "PK\x03\x04papers"},
		{"page.html", "text/html; charset=utf-8", "<!doctype html><script>alert(1)</script>"},
	} {
		_, _, d := answerWith(c.name, c.media, c.body)
		a := read(d)
		if a.status != http.StatusOK || string(a.body) != c.body {
			t.Fatalf("%s: %d, %q", c.name, a.status, a.body)
		}
		for h, want := range map[string]string{
			"Content-Type":            "application/octet-stream",
			"Content-Disposition":     "attachment",
			"X-Content-Type-Options":  "nosniff",
			"Content-Security-Policy": "sandbox; default-src 'none'",
		} {
			if got := a.header.Get(h); got != want {
				t.Errorf("%s: %s is %q, want %q", c.name, h, got, want)
			}
		}
	}

	msg, kept, first := answerWith("first.pdf", "application/pdf", "%PDF-1.7 first")
	redact(kept.ID)
	if !redacted(kept.ID) || redacted(msg.ID) || read(first).status != http.StatusNotFound {
		t.Fatalf("redacting files.kept alone: kept %t, message %t, file %d", redacted(kept.ID), redacted(msg.ID), read(first).status)
	}
	msg, kept, second := answerWith("second.pdf", "application/pdf", "%PDF-1.7 second")
	before := len(f.log(s.ID))
	redact(msg.ID)
	if !redacted(msg.ID) || !redacted(kept.ID) || read(second).status != http.StatusNotFound {
		t.Fatalf("redacting the answer: message %t, kept %t, file %d", redacted(msg.ID), redacted(kept.ID), read(second).status)
	}
	if after := f.log(s.ID); len(after) != before+2 || after[before].Type != session.TypeEventRedacted || after[before+1].Type != session.TypeEventRedacted {
		t.Fatalf("redacting the answer appended %d events", len(after)-before)
	}
}

// TestTheDocumentStatesKeptImages: the OpenAPI document states files.kept
// on the event routes, the blob route's headers, and the redaction of an
// answer's images with it (spec 055).
func TestTheDocumentStatesKeptImages(t *testing.T) {
	raw, err := os.ReadFile(committed)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Description string `yaml:"description"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, method, want string }{
		{"/sessions/{id}/events", "get", KeptRule},
		{"/sessions/{id}/stream", "get", KeptRule},
		{"/sessions/{id}/events", "get", MachineRule},
		{"/sessions/{id}/stream", "get", MachineRule},
		{"/sessions/{id}/events", "get", "restarted for the machine the session had"},
		{"/sessions/{id}/events", "get", "replaced for a new, empty one"},
		{"/sessions/{id}/events", "get", "code: machine_lost"},
		{"/sessions/{id}/blobs/{digest}", "get", "nosniff"},
		{"/sessions/{id}/blobs/{digest}", "get", blobCache},
		{"/sessions/{id}/blobs/{digest}", "get", "Content-Disposition inline"},
		{"/sessions/{id}/blobs/{digest}", "get", "a file an answer linked"},
		{"/sessions/{id}/events", "get", "not_a_file"},
		{"/sessions/{id}/events", "get", "sandbox:"},
		{"/sessions/{id}/files", "get", strconv.Itoa(MaxFileBytes) + " bytes is file_too_large"},
		{"/sessions/{id}/events", "get", strconv.Itoa(harness.MaxKeptFileBytes) + " bytes, " + strconv.Itoa(harness.MaxKeptFilesBytes) + " bytes together"},
		{"/sessions/{id}/events/{event_id}/redact", "post", "redacts the files.kept that names it in the same append"},
	} {
		if !strings.Contains(doc.Paths[c.path][c.method].Description, c.want) {
			t.Errorf("%s %s does not state %q", c.method, c.path, c.want)
		}
	}
}
