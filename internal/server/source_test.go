// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/arcastub"
)

// sourceFixture is a folder fixture whose files API answers a read of
// any file up to MaxSourceBytes inline, as an Arca-compatible files API
// does, and which keeps the send questions its authorizer was asked.
type sourceFixture struct {
	*folderFixture
	mu    sync.Mutex
	sends []authz.Request
}

func newSourceFixture(t *testing.T) *sourceFixture {
	t.Helper()
	sf := &sourceFixture{folderFixture: newFolderFixture(t)}
	sf.arca.RedirectAbove(MaxSourceBytes)
	named := sf.authz.answer
	sf.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionSend {
			sf.mu.Lock()
			sf.sends = append(sf.sends, req)
			sf.mu.Unlock()
		}
		return named(req)
	}
	sf.apply("alice", "reviewer", "Review.")
	return sf
}

// lastSend is the last session.send question asked.
func (sf *sourceFixture) lastSend() authz.Request {
	sf.t.Helper()
	sf.mu.Lock()
	defer sf.mu.Unlock()
	if len(sf.sends) == 0 {
		sf.t.Fatal("session.send was never asked")
	}
	return sf.sends[len(sf.sends)-1]
}

// send sends alice's message to session id attaching the files, each
// one JSON object, and answers its event when it is taken.
func (f *fixture) sendFiles(id string, files ...string) (answer, session.Event) {
	f.t.Helper()
	a := f.sendMessage(id, `{"content":[{"type":"text","text":"Look."}],"attachments":[`+strings.Join(files, ",")+`]}`)
	var ev session.Event
	if a.status == http.StatusOK {
		a.decode(f.t, &ev)
	}
	return a, ev
}

// source is one file of attachments named by its source.
func source(name, owner, path string, version ...int) string {
	src := map[string]any{"owner": owner, "path": path}
	if len(version) > 0 {
		src["version"] = version[0]
	}
	b, err := json.Marshal(map[string]any{"name": name, "source": src})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// records are the files a user.message event records.
func records(t *testing.T, ev session.Event) []session.Attachment {
	t.Helper()
	var p session.UserMessage
	if err := ev.Decode(&p); err != nil {
		t.Fatal(err)
	}
	return p.Attachments
}

// TestASourceIsReadWithTheSendersToken: a file a message names by its
// source is read at the files API of the session's folder with the send's
// own bearer, at ?version=N when it names a version, written to the
// session's folder with the session's own token as a data file is, and
// recorded with its source; its media type is the files API's, or the
// bytes' where the files API names none or application/octet-stream
// (spec 079).
func TestASourceIsReadWithTheSendersToken(t *testing.T) {
	f := newSourceFixture(t)
	s := f.create("alice", "reviewer")
	const q3 = "files/Reports/q3 final.csv"
	now, then, pdf := []byte("month,total\n7,8\n"), []byte("month,total\n1,2\n"), []byte("%PDF-1.7\n1 0 obj\n")
	f.arca.Put("me", q3, arcastub.Object{Data: now, ContentType: "text/csv"})
	f.arca.PutVersion("me", q3, 3, arcastub.Object{Data: then, ContentType: "text/csv"})
	f.arca.Put("me", "files/scan", arcastub.Object{Data: pdf, ContentType: "application/octet-stream"})
	f.arca.Put("owner-7", "files/shared/a.txt", arcastub.Object{Data: []byte("shared\n")})
	a, ev := f.sendFiles(s.ID, source("q3.csv", "me", q3), source("q3-then.csv", "me", q3, 3), source("scan.pdf", "me", "files/scan"),
		source("a.txt", "owner-7", "files/shared/a.txt"), `{"name":"inline.txt","data":"`+b64("data\n")+`"}`)
	if a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	at := func(name string) string { return session.AttachmentPath(ev.ID, name) }
	want := []session.Attachment{
		{Name: "q3.csv", MediaType: "text/csv", Size: int64(len(now)), Blob: session.DigestOf(now), Path: at("q3.csv"), Ref: at("q3.csv"), Source: session.AttachmentSource{Owner: "me", Path: q3}, Inline: true},
		{Name: "q3-then.csv", MediaType: "text/csv", Size: int64(len(then)), Blob: session.DigestOf(then), Path: at("q3-then.csv"), Ref: at("q3-then.csv"), Source: session.AttachmentSource{Owner: "me", Path: q3, Version: 3}, Inline: true},
		{Name: "scan.pdf", MediaType: "application/pdf", Size: int64(len(pdf)), Blob: session.DigestOf(pdf), Path: at("scan.pdf"), Ref: at("scan.pdf"), Source: session.AttachmentSource{Owner: "me", Path: "files/scan"}},
		{Name: "a.txt", MediaType: "text/plain; charset=utf-8", Size: 7, Blob: session.DigestOf([]byte("shared\n")), Path: at("a.txt"), Ref: at("a.txt"), Source: session.AttachmentSource{Owner: "owner-7", Path: "files/shared/a.txt"}, Inline: true},
		{Name: "inline.txt", MediaType: "text/plain; charset=utf-8", Size: 5, Blob: session.DigestOf([]byte("data\n")), Path: at("inline.txt"), Ref: at("inline.txt"), Inline: true},
	}
	got := records(t, ev)
	if len(got) != len(want) {
		t.Fatalf("the records %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record %d %+v, want %+v", i, got[i], want[i])
		}
	}
	for i, data := range [][]byte{now, then, pdf} {
		o, ok := f.arca.Object(space, folderPath(s.ID)+"/"+want[i].Ref)
		if !ok || !bytes.Equal(o.Data, data) || o.Token != "session:"+s.ID {
			t.Fatalf("the file %s in the folder %+v %t", want[i].Name, o, ok)
		}
	}
	var reads []arcastub.Recorded
	for _, r := range f.arca.Requests() {
		if r.Op == arcastub.OpRead {
			reads = append(reads, r)
		}
	}
	paths := []string{"/v1/files/me/files/Reports/q3%20final.csv", "/v1/files/me/files/Reports/q3%20final.csv", "/v1/files/me/files/scan", "/v1/files/owner-7/files/shared/a.txt"}
	if len(reads) != len(paths) {
		t.Fatalf("the reads %+v", reads)
	}
	for i, r := range reads {
		if r.Path != paths[i] || r.Header.Get("Authorization") != "Bearer alice" || (r.Query == "version=3") != (i == 1) {
			t.Errorf("read %d is %s?%s with %q", i, r.Path, r.Query, r.Header.Get("Authorization"))
		}
	}
	if ms := records(t, f.log(s.ID)[len(f.log(s.ID))-1]); len(ms) != len(want) {
		t.Fatalf("the log's last message records %+v", ms)
	}
}

// TestASourceTheFilesAPIRefuses: a source the files API answers 401, 403
// or 404 for is attachment_source_not_found, one answer for the three; a
// redirect is attachment_too_large; any other failure files_unavailable;
// and a refused message appends nothing and writes nothing to the
// session's folder (spec 079).
func TestASourceTheFilesAPIRefuses(t *testing.T) {
	f := newSourceFixture(t)
	s := f.create("alice", "reviewer")
	f.arca.Put("me", "files/a.csv", arcastub.Object{Data: []byte("a\n"), ContentType: "text/csv"})
	last := f.stored(s.ID).LastSeq
	refused := func(what string, status int, code string) answer {
		t.Helper()
		a, _ := f.sendFiles(s.ID, source("a.csv", "me", "files/a.csv"), `{"name":"b.txt","data":"`+b64("b")+`"}`)
		if a.status != status || a.code() != code {
			t.Fatalf("%s: %d %s", what, a.status, a.body)
		}
		if got := f.stored(s.ID).LastSeq; got != last {
			t.Fatalf("%s appended to seq %d", what, got)
		}
		if p := f.arca.Paths(space, folderPath(s.ID)+"/"); len(p) != 0 {
			t.Fatalf("%s wrote %v", what, p)
		}
		return a
	}
	f.arca.Fail(arcastub.OpRead, arcastub.Failure{Status: http.StatusUnauthorized, Code: "unauthenticated"})
	unauthenticated := refused("a 401", http.StatusNotFound, CodeAttachmentSourceNotFound)
	f.arca.Fail(arcastub.OpRead, arcastub.Failure{Status: http.StatusForbidden, Code: "forbidden"})
	forbidden := refused("a 403", http.StatusNotFound, CodeAttachmentSourceNotFound)
	f.arca.Remove("me", "files/a.csv")
	missing := refused("a 404", http.StatusNotFound, CodeAttachmentSourceNotFound)
	if !bytes.Equal(unauthenticated.body, forbidden.body) || !bytes.Equal(forbidden.body, missing.body) {
		t.Fatalf("the three answers differ:\n%s\n%s\n%s", unauthenticated.body, forbidden.body, missing.body)
	}
	f.arca.Put("me", "files/a.csv", arcastub.Object{Data: []byte("a\n"), ContentType: "text/csv"})
	f.arca.RedirectAbove(-1)
	if a := refused("a redirect", http.StatusRequestEntityTooLarge, CodeAttachmentTooLarge); !strings.Contains(string(a.body), "redirect") {
		t.Fatalf("a redirect's detail %s", a.body)
	}
	f.arca.RedirectAbove(MaxSourceBytes)
	f.arca.Fail(arcastub.OpRead, arcastub.Failure{Status: http.StatusInternalServerError, Code: "internal"})
	refused("a failure", http.StatusServiceUnavailable, CodeFilesUnavailable)
	for _, r := range f.arca.Requests() {
		if r.Op == "bucket-read" {
			t.Fatal("a redirect was followed")
		}
	}
	if a, _ := f.sendFiles(s.ID, source("a.csv", "me", "files/a.csv")); a.status != http.StatusOK {
		t.Fatalf("the source once the files API answers: %d %s", a.status, a.body)
	}
}

// TestASourcesRules: a file names exactly one of data, a blob and a
// source, a source names no media_type, an owner and a path within their
// rules and a version from 1; a session without a folder, and a server
// whose files store reads no source, refuse a source; sources count
// toward MaxAttachments (spec 079).
func TestASourcesRules(t *testing.T) {
	f := newSourceFixture(t)
	s := f.create("alice", "reviewer")
	f.arca.Put("me", "files/a.csv", arcastub.Object{Data: []byte("a\n")})
	for _, file := range []string{
		`{"name":"a","source":{"owner":"me","path":"files/a.csv"},"data":"` + b64("x") + `"}`,
		`{"name":"a","source":{"owner":"me","path":"files/a.csv"},"blob":"` + string(session.DigestOf([]byte("x"))) + `"}`,
		`{"name":"a","source":{"owner":"me","path":"files/a.csv"},"media_type":"text/plain"}`,
		`{"name":"a","source":{"owner":"me","path":"files/a.csv","plane":"files"}}`,
		source("a", "", "files/a.csv"),
		source("a", strings.Repeat("o", MaxAttachmentName+1), "files/a.csv"),
		source("a", "me\a", "files/a.csv"),
		source("a", "me", ""),
		source("a", "me", "/files/a.csv"),
		source("a", "me", "files//a.csv"),
		source("a", "me", "files/./a.csv"),
		source("a", "me", "files/../a.csv"),
		source("a", "me", "files/a/"),
		source("a", "me", "files/"+strings.Repeat("x", MaxSourcePath)),
		source("a", "me", "files/a\u202e.csv"),
		source("a", "me", "files/a.csv", 0),
		source("a", "me", "files/a.csv", -1),
	} {
		if a, _ := f.sendFiles(s.ID, file); a.status != http.StatusBadRequest || a.code() != CodeInvalidRequest {
			t.Errorf("%s: %d %s", file, a.status, a.body)
		}
	}
	many := make([]string, MaxAttachments+1)
	for i := range many {
		many[i] = source("f"+strconv.Itoa(i), "me", "files/a.csv")
	}
	if a, _ := f.sendFiles(s.ID, many...); a.status != http.StatusBadRequest || !strings.Contains(string(a.body), "at most "+strconv.Itoa(MaxAttachments)) {
		t.Fatalf("too many sources: %d %s", a.status, a.body)
	}
	if a, _ := f.sendFiles(s.ID, many[:MaxAttachments]...); a.status != http.StatusOK {
		t.Fatalf("as many sources as a message holds: %d %s", a.status, a.body)
	}

	f.named = false
	bare := f.create("alice", "reviewer")
	if a, _ := f.sendFiles(bare.ID, source("a.csv", "me", "files/a.csv")); a.status != http.StatusBadRequest || a.code() != CodeInvalidRequest {
		t.Fatalf("a session without a folder: %d %s", a.status, a.body)
	}
	f.api.o.Files.Source = nil
	if a, _ := f.sendFiles(s.ID, source("a.csv", "me", "files/a.csv")); a.status != http.StatusServiceUnavailable || a.code() != CodeFilesUnavailable {
		t.Fatalf("a server that reads no source: %d %s", a.status, a.body)
	}
}

// TestASourceCountsAsAnAttachment: the send's question counts a source
// among the message's attachments, beside its data files and its images
// (spec 061).
func TestASourceCountsAsAnAttachment(t *testing.T) {
	f := newSourceFixture(t)
	s := f.create("alice", "reviewer")
	f.arca.Put("me", "files/a.csv", arcastub.Object{Data: []byte("a\n")})
	if a, _ := f.sendFiles(s.ID, source("a.csv", "me", "files/a.csv"), `{"name":"b.txt","data":"`+b64("b")+`"}`); a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	if got := f.lastSend().Resource.Fields["attachments"]; got != 2 {
		t.Fatalf("the send question counts %v attachments", got)
	}
}

// TestAForksMessageReadsASource: a fork's message that names a source
// is read with the fork request's bearer at the fork's own folder once
// its send is allowed, kept in the fork's folder and counted by its send
// question; a file the fork keeps by its blob keeps its record's source
// and mark; a refused read leaves no fork (spec 079).
func TestAForksMessageReadsASource(t *testing.T) {
	f := newSourceFixture(t)
	parent := f.create("alice", "reviewer")
	f.turn(parent.ID, 1, "Hello.", 1)
	f.arca.Put("me", "files/a.csv", arcastub.Object{Data: []byte("a,b\n"), ContentType: "text/csv"})
	f.arca.Put("me", "files/b.md", arcastub.Object{Data: []byte("# b\n"), ContentType: "text/markdown"})
	_, sent := f.sendFiles(parent.ID, source("a.csv", "me", "files/a.csv"))
	kept := records(t, sent)[0]
	f.turn(parent.ID, 2, "Read it.", 1)
	forks := func() int {
		var page struct {
			Items []session.Session `json:"items"`
		}
		f.do(http.MethodGet, "/v1/sessions?parent="+parent.ID, "alice", "").decode(t, &page)
		return len(page.Items)
	}
	body := `{"message":{"content":[{"type":"text","text":"Again."}],"attachments":[{"name":"a.csv","blob":"` + string(kept.Blob) + `"},` + source("b.md", "me", "files/b.md") + `]}}`

	f.arca.Fail(arcastub.OpRead, arcastub.Failure{Status: http.StatusForbidden, Code: "forbidden"})
	if a := f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/fork", "alice", body); a.status != http.StatusNotFound || a.code() != CodeAttachmentSourceNotFound {
		t.Fatalf("a fork whose source is refused: %d %s", a.status, a.body)
	}
	if n := forks(); n != 0 {
		t.Fatalf("a refused fork left %d forks", n)
	}

	before := len(f.arca.Requests())
	fork := f.forked(parent.ID, body)
	evs := f.log(fork.ID)
	last := evs[len(evs)-1]
	got := records(t, last)
	at := func(name string) string { return session.AttachmentPath(last.ID, name) }
	want := []session.Attachment{
		{Name: "a.csv", MediaType: "text/csv", Size: 4, Blob: kept.Blob, Path: at("a.csv"), Ref: kept.Ref, Source: session.AttachmentSource{Owner: "me", Path: "files/a.csv"}, Inline: true},
		{Name: "b.md", MediaType: "text/markdown", Size: 4, Blob: session.DigestOf([]byte("# b\n")), Path: at("b.md"), Ref: at("b.md"), Source: session.AttachmentSource{Owner: "me", Path: "files/b.md"}, Inline: true},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the fork's message records %+v, want %+v", got, want)
	}
	if o, ok := f.arca.Object(space, folderPath(fork.ID)+"/"+want[1].Ref); !ok || string(o.Data) != "# b\n" || o.Token != "session:"+fork.ID {
		t.Fatalf("the fork's folder holds %+v %t", o, ok)
	}
	var read *arcastub.Recorded
	for _, r := range f.arca.Requests()[before:] {
		if r.Op == arcastub.OpRead {
			read = &r
		}
	}
	if read == nil || read.Path != "/v1/files/me/files/b.md" || read.Header.Get("Authorization") != "Bearer alice" {
		t.Fatalf("the fork's read %+v", read)
	}
	if got := f.lastSend().Resource.Fields["attachments"]; got != 2 {
		t.Fatalf("the fork's send question counts %v attachments", got)
	}
}

// TestATextFileIsMarkedInline: a message's text-like UTF-8 file of at
// most session.InlineTextBytes is marked inline, in the message's order
// while the marked files stay within session.InlineMessageTextBytes; a
// larger file, a binary one and one that is not UTF-8 are not (spec
// 079).
func TestATextFileIsMarkedInline(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	full := strings.Repeat("x", session.InlineTextBytes)
	data := func(name, media, body string) string {
		b, err := json.Marshal(map[string]string{"name": name, "media_type": media, "data": b64(body)})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	_, ev := f.sendFiles(s.ID, data("a.csv", "", "a,b\n"), data("big.txt", "", full+"x"), data("nul.txt", "text/plain", "a\x00b"),
		data("latin.txt", "text/plain", "caf\xe9"), data("cfg", "application/json", "{}"), data("photo.png", "image/png", pngBytes))
	marks := func(ev session.Event) []bool {
		var out []bool
		for _, a := range records(t, ev) {
			out = append(out, a.Inline)
		}
		return out
	}
	if got, want := marks(ev), []bool{true, false, false, false, true, false}; !slices.Equal(got, want) {
		t.Fatalf("the marks %v, want %v", got, want)
	}
	_, ev = f.sendFiles(s.ID, data("1.txt", "", full), data("2.txt", "", full), data("3.txt", "", full[:session.InlineTextBytes-1]), data("4.txt", "", "xy"), data("5.txt", "", "z"))
	if got, want := marks(ev), []bool{true, true, true, false, true}; !slices.Equal(got, want) {
		t.Fatalf("the marks within the message's bound %v, want %v", got, want)
	}
}
