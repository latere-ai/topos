// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/internal/filestore"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/arcastub"
)

// space is the space the folders of these tests are in.
const space = "https://issuer.example|alice"

// folderFixture is a fixture whose server keeps a files store at an
// Arca stub, written with a token naming the session and read with the
// server's own, and whose authorizer names a folder for each session it
// allows to be created or forked, unless named is cleared.
type folderFixture struct {
	*fixture
	arca  *arcastub.Server
	named bool
	// spool is the server's spool directory (spec 081).
	spool string
}

func newFolderFixture(t *testing.T) *folderFixture {
	t.Helper()
	// The bucket is on https, the one scheme a source's read follows a
	// redirect to (spec 081).
	arca := arcastub.NewTLS(t)
	open := func(tok func(session.Session) string) func(context.Context, session.Session) (harness.Files, error) {
		return func(_ context.Context, s session.Session) (harness.Files, error) {
			return filestore.New(*s.Folder, func(context.Context) (string, error) { return tok(s), nil }, arca.Client())
		}
	}
	ff := &folderFixture{arca: arca, named: true, spool: filepath.Join(t.TempDir(), "spool")}
	ff.fixture = newFixture(t, func(o *Options) {
		o.Files = &Files{Write: open(func(s session.Session) string { return "session:" + s.ID }), Read: open(func(session.Session) string { return "server" }),
			Token: func(_ context.Context, s session.Session) (string, error) { return "session:" + s.ID, nil },
			Source: func(ctx context.Context, root, token string, src session.AttachmentSource, max int64) (*filestore.Source, error) {
				return filestore.ReadSource(ctx, arca.Client(), root, token, src, max)
			}, Spool: ff.spool}
	})
	ff.authz.answer = func(req authz.Request) (authz.Decision, error) {
		d, err := ff.authz.next.Authorize(context.Background(), req)
		if err != nil || !d.Allow || (req.Action != authorizer.ActionSessionCreate && req.Action != authorizer.ActionSessionFork) || !ff.named {
			return d, err
		}
		limits, err := json.Marshal(map[string]any{"folder": map[string]string{"url": arca.URL(), "owner": space, "path": folderPath(req.Resource.String("session_id"))}})
		d.Limits = limits
		return d, err
	}
	return ff
}

func folderPath(id string) string { return "files/sessions/" + id }

// stored is the session as its store holds it.
func (f *fixture) stored(id string) session.Session {
	f.t.Helper()
	s, err := f.sessions.Get(f.t.Context(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

// keep appends an answer of turn that links a file kept at ref with the
// bytes data, which the test writes into the folder first.
func (ff *folderFixture) keep(id string, turn int, ref, data string) session.Digest {
	ff.t.Helper()
	ff.arca.Put(space, folderPath(id)+"/"+ref, arcastub.Object{Data: []byte(data)})
	msg, err := session.NewEvent(session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockText, Text: "[Report](report.pdf)"}}}}, time.Now())
	if err != nil {
		ff.t.Fatal(err)
	}
	d := session.DigestOf([]byte(data))
	kept, err := session.NewEvent(session.TypeFilesKept, session.FilesKept{Message: msg.ID, Files: []session.KeptFile{{Path: "report.pdf", Resolved: "/workspace/report.pdf", Blob: d, MediaType: "application/pdf", Size: int64(len(data)), Ref: ref}}, Skipped: []session.SkippedFile{}}, time.Now())
	if err != nil {
		ff.t.Fatal(err)
	}
	idle, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopEndTurn}, time.Now())
	if err != nil {
		ff.t.Fatal(err)
	}
	batch := []session.Event{msg, kept, idle}
	for i := range batch {
		batch[i].Turn = turn
	}
	s := ff.stored(id)
	session.Stamp(id, s.LastSeq, batch)
	if _, err := ff.sessions.Append(ff.t.Context(), id, s.LastSeq, batch); err != nil {
		ff.t.Fatal(err)
	}
	return d
}

// attach sends a message attaching name with data and answers its event.
func (ff *folderFixture) attach(id, name, data string) (answer, session.Event) {
	ff.t.Helper()
	a := ff.sendMessage(id, `{"content":[{"type":"text","text":"See the file."}],"attachments":[{"name":"`+name+`","media_type":"text/plain","data":"`+b64(data)+`"}]}`)
	var ev session.Event
	if a.status == http.StatusOK {
		a.decode(ff.t, &ev)
	}
	return a, ev
}

// TestTheAllowsFolderIsKeptAndAnsweredOnceUsed: the folder an allow of
// a create names is kept with the session, its address included, and the
// session answers it as {owner, path} once a file is stored there and
// never with its address; a server with no files store refuses an allow
// that names a folder as authorizer_unavailable (spec 068).
func TestTheAllowsFolderIsKeptAndAnsweredOnceUsed(t *testing.T) {
	f := newFolderFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	if s.Folder != nil {
		t.Fatalf("a session with no stored file answers its folder %+v", s.Folder)
	}
	want := session.Folder{URL: f.arca.URL(), Owner: space, Path: folderPath(s.ID)}
	if got := f.stored(s.ID).Folder; got == nil || *got != want {
		t.Fatalf("the stored folder %+v, want %+v", got, want)
	}
	if a, _ := f.attach(s.ID, "notes.txt", "notes"); a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	a := f.do(http.MethodGet, "/v1/sessions/"+s.ID, "alice", "")
	var got session.Session
	a.decode(t, &got)
	if got.Folder == nil || *got.Folder != (session.Folder{Owner: space, Path: folderPath(s.ID)}) || strings.Contains(string(a.body), f.arca.URL()) || strings.Contains(string(a.body), `"stored"`) {
		t.Fatalf("the session answers %s", a.body)
	}
	list := f.do(http.MethodGet, "/v1/sessions", "alice", "")
	if !strings.Contains(string(list.body), `"folder":{"owner"`) || strings.Contains(string(list.body), f.arca.URL()) {
		t.Fatalf("the list answers %s", list.body)
	}

	none := newFixture(t)
	none.apply("alice", "reviewer", "Review.")
	none.authz.answer = func(req authz.Request) (authz.Decision, error) {
		d, err := none.authz.next.Authorize(context.Background(), req)
		if d.Allow && req.Action == authorizer.ActionSessionCreate {
			d.Limits = json.RawMessage(`{"folder":{"url":"https://files.example.com/v1","owner":"o","path":"files/x"}}`)
		}
		return d, err
	}
	if a := none.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer"}`); a.status != http.StatusServiceUnavailable || a.code() != "authorizer_unavailable" {
		t.Fatalf("a folder on a server with no files store: %d %s", a.status, a.body)
	}
}

// TestAttachmentsGoToTheSessionsFolder: a message's file is written to
// the session's folder at its path under the message's event id, with
// the session's own token, create-only, and its record names it by ref
// beside its digest; a write refused for its size is
// attachment_too_large, any other failure files_unavailable, and a
// refused message appends nothing (spec 068).
func TestAttachmentsGoToTheSessionsFolder(t *testing.T) {
	f := newFolderFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	a, ev := f.attach(s.ID, "sales.csv", "month,total\n")
	if a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	var p session.UserMessage
	if err := ev.Decode(&p); err != nil {
		t.Fatal(err)
	}
	path := "attachments/" + ev.ID + "/sales.csv"
	want := session.Attachment{Name: "sales.csv", MediaType: "text/plain", Size: 12, Blob: session.DigestOf([]byte("month,total\n")), Path: path, Ref: path, Inline: true}
	if len(p.Attachments) != 1 || p.Attachments[0] != want {
		t.Fatalf("the record %+v, want %+v", p.Attachments, want)
	}
	o, ok := f.arca.Object(space, folderPath(s.ID)+"/"+path)
	if !ok || string(o.Data) != "month,total\n" || o.Token != "session:"+s.ID || o.ContentType != "text/plain" {
		t.Fatalf("the file in the folder %+v %t", o, ok)
	}
	if _, err := f.sessions.Blob(t.Context(), s.ID, want.Blob); err == nil {
		t.Fatal("the file is a blob of the session too")
	}
	last := f.stored(s.ID).LastSeq
	for failure, code := range map[arcastub.Failure]string{
		{Status: http.StatusRequestEntityTooLarge, Code: "object_too_large"}: CodeAttachmentTooLarge,
		{Status: http.StatusForbidden, Code: "forbidden"}:                    CodeFilesUnavailable,
	} {
		f.arca.Fail(arcastub.OpPut, failure)
		if a, _ := f.attach(s.ID, "more.csv", "x"); a.status == http.StatusOK || a.code() != code {
			t.Fatalf("a write refused %s: %d %s", failure.Code, a.status, a.body)
		}
	}
	if got := f.stored(s.ID).LastSeq; got != last {
		t.Fatalf("a refused message appended: last_seq %d, was %d", got, last)
	}
}

// TestTheBlobRouteStreamsAFileOfTheFolder: a digest the folder holds a
// file of is read from there with the server's own token, as a stream
// with its length, the media type its record names and the blob route's
// headers; a message's attachment is labeled with its own media type
// too; a blob of the session's
// own store is read there as before; a redacted file's digest is
// not_found and the file is gone from the folder (spec 073), a file gone
// from the folder not_found, and a read the files service fails
// files_unavailable (spec 068).
func TestTheBlobRouteStreamsAFileOfTheFolder(t *testing.T) {
	f := newFolderFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	_, ev := f.attach(s.ID, "notes.txt", "my notes")
	report := f.keep(s.ID, 1, "results/1/out/report.pdf", "%PDF-1.7 report")
	blob := func(d session.Digest) answer {
		return f.do(http.MethodGet, "/v1/sessions/"+s.ID+"/blobs/"+string(d), "alice", "")
	}
	a := blob(report)
	if a.status != http.StatusOK || string(a.body) != "%PDF-1.7 report" || a.header.Get("Content-Length") != strconv.Itoa(len("%PDF-1.7 report")) ||
		a.header.Get("Content-Type") != "application/pdf" || a.header.Get("Content-Disposition") != "attachment" || a.header.Get("ETag") != strconv.Quote(string(report)) ||
		a.header.Get("X-Content-Type-Options") != "nosniff" || a.header.Get("Cache-Control") != blobCache {
		t.Fatalf("the kept file: %d %v %q", a.status, a.header, a.body)
	}
	var reads []arcastub.Recorded
	for _, r := range f.arca.Requests() {
		if r.Op == arcastub.OpRead {
			reads = append(reads, r)
		}
	}
	if len(reads) != 1 || reads[0].Header.Get("Authorization") != "Bearer server" {
		t.Fatalf("the reads %+v", reads)
	}
	var p session.UserMessage
	if err := ev.Decode(&p); err != nil {
		t.Fatal(err)
	}
	if a := blob(p.Attachments[0].Blob); a.status != http.StatusOK || string(a.body) != "my notes" ||
		a.header.Get("Content-Type") != "text/plain" || a.header.Get("Content-Length") != strconv.Itoa(len("my notes")) {
		t.Fatalf("the attachment: %d %v %q", a.status, a.header, a.body)
	}
	if a := blob(f.stored(s.ID).Agent.Bundle); a.status != http.StatusOK {
		t.Fatalf("the agent's bundle, a blob: %d %v", a.status, a.header)
	}
	f.arca.Fail(arcastub.OpRead, arcastub.Failure{Status: http.StatusServiceUnavailable, Code: "storage_unavailable"})
	if a := blob(report); a.status != http.StatusServiceUnavailable || a.code() != CodeFilesUnavailable {
		t.Fatalf("a read the files service fails: %d %s", a.status, a.body)
	}
	evs, err := f.sessions.Events(t.Context(), s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var keptID string
	for _, e := range evs {
		var k session.FilesKept
		if e.Type == session.TypeFilesKept && e.Decode(&k) == nil && k.Files[0].Blob == report {
			keptID = e.ID
		}
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events/"+keptID+"/redact", "alice", `{}`); a.status != http.StatusNoContent {
		t.Fatalf("redact: %d %s", a.status, a.body)
	}
	if a := blob(report); a.status != http.StatusNotFound {
		t.Fatalf("a redacted file: %d %s", a.status, a.body)
	}
	if _, ok := f.arca.Object(space, folderPath(s.ID)+"/results/1/out/report.pdf"); ok {
		t.Fatal("the redacted file is still in the folder")
	}
	missing := f.keep(s.ID, 3, "results/3/missing.pdf", "%PDF missing")
	f.arca.Remove(space, folderPath(s.ID)+"/results/3/missing.pdf")
	if a := blob(missing); a.status != http.StatusNotFound {
		t.Fatalf("a file gone from the folder: %d %s", a.status, a.body)
	}
	f.api.o.Files = nil
	if a := blob(missing); a.status != http.StatusServiceUnavailable || a.code() != CodeFilesUnavailable {
		t.Fatalf("a file of a folder on a server with no files store: %d %s", a.status, a.body)
	}
}

// TestAForkCopiesItsFoldersOnTheServer: a fork copies each folder of its
// parent's that its copied events and its message name into its own,
// with its own token, before its log is written, writes its message's new
// files there, and reads every file from its own folder; a fork whose
// allow names no folder of a parent with files, and one whose copy fails,
// are files_unavailable and leave no fork (spec 068).
func TestAForkCopiesItsFoldersOnTheServer(t *testing.T) {
	f := newFolderFixture(t)
	f.apply("alice", "reviewer", "Review.")
	parent := f.create("alice", "reviewer")
	f.turn(parent.ID, 1, "Hello.", 1)
	_, ev := f.attach(parent.ID, "notes.txt", "my notes")
	var p session.UserMessage
	if err := ev.Decode(&p); err != nil {
		t.Fatal(err)
	}
	notes := p.Attachments[0].Blob
	report := f.keep(parent.ID, 2, "results/2/report.pdf", "%PDF-1.7 report")
	later := f.keep(parent.ID, 3, "results/3/later.pdf", "%PDF later")

	forks := func() int {
		var out []session.Session
		a := f.do(http.MethodGet, "/v1/sessions?parent="+parent.ID, "alice", "")
		var page struct {
			Items []session.Session `json:"items"`
		}
		a.decode(t, &page)
		out = page.Items
		return len(out)
	}
	evs, err := f.sessions.Events(t.Context(), parent.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var at uint64
	for _, e := range evs {
		if e.Type == session.TypeSessionStatus && e.Turn == 2 {
			at = e.Seq
		}
	}
	a := f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/fork", "alice",
		`{"at_seq":`+strconv.FormatUint(at, 10)+`,"message":{"content":[{"type":"text","text":"Again."}],"attachments":[{"name":"kept.txt","blob":"`+string(notes)+`"},{"name":"new.txt","data":"`+b64("new file")+`"}]}}`)
	if a.status != http.StatusCreated {
		t.Fatalf("fork: %d %s", a.status, a.body)
	}
	var fork session.Session
	a.decode(t, &fork)
	if fork.Folder == nil || fork.Folder.Path != folderPath(fork.ID) {
		t.Fatalf("the fork answers the folder %+v", fork.Folder)
	}
	var copies []string
	for _, r := range f.arca.Requests() {
		if r.Op == arcastub.OpCopy {
			if r.Header.Get("Authorization") != "Bearer session:"+fork.ID {
				t.Fatalf("a copy carried %q", r.Header.Get("Authorization"))
			}
			copies = append(copies, r.Path[strings.Index(r.Path, folderPath(parent.ID))+len(folderPath(parent.ID))+1:])
		}
	}
	slices.Sort(copies)
	if !slices.Equal(copies, []string{"attachments/" + ev.ID, "results/2"}) {
		t.Fatalf("the fork copied %v", copies)
	}
	if got := f.arca.Paths(space, folderPath(fork.ID)+"/results/"); !slices.Equal(got, []string{folderPath(fork.ID) + "/results/2/report.pdf"}) {
		t.Fatalf("the fork's results %v; a later turn's are not its", got)
	}
	for d, want := range map[session.Digest]string{notes: "my notes", report: "%PDF-1.7 report", session.DigestOf([]byte("new file")): "new file"} {
		if a := f.do(http.MethodGet, "/v1/sessions/"+fork.ID+"/blobs/"+string(d), "alice", ""); a.status != http.StatusOK || string(a.body) != want {
			t.Fatalf("the fork's %s: %d %q", want, a.status, a.body)
		}
	}
	if a := f.do(http.MethodGet, "/v1/sessions/"+fork.ID+"/blobs/"+string(later), "alice", ""); a.status != http.StatusNotFound {
		t.Fatalf("a file of a turn after the fork point: %d", a.status)
	}
	fevs, err := f.sessions.Events(t.Context(), fork.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var m session.UserMessage
	if err := fevs[len(fevs)-1].Decode(&m); err != nil || len(m.Attachments) != 2 || m.Attachments[0].Ref != p.Attachments[0].Ref ||
		m.Attachments[1].Ref != m.Attachments[1].Path || !strings.HasPrefix(m.Attachments[1].Ref, "attachments/"+fevs[len(fevs)-1].ID+"/") {
		t.Fatalf("the fork's message records %+v, %v", m.Attachments, err)
	}

	before := forks()
	f.arca.Fail(arcastub.OpCopy, arcastub.Failure{Status: http.StatusServiceUnavailable, Code: "storage_unavailable"})
	if a := f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/fork", "alice", ``); a.status != http.StatusServiceUnavailable || a.code() != CodeFilesUnavailable {
		t.Fatalf("a fork whose copy fails: %d %s", a.status, a.body)
	}
	f.named = false
	if a := f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/fork", "alice", ``); a.status != http.StatusServiceUnavailable || a.code() != CodeFilesUnavailable {
		t.Fatalf("a fork with no folder of a parent with files: %d %s", a.status, a.body)
	}
	if forks() != before {
		t.Fatal("a refused fork was left")
	}
	f.named = true
	f.arca.Put(space, folderPath(parent.ID)+"/results/9/none", arcastub.Object{})
	bare := f.create("alice", "reviewer")
	f.turn(bare.ID, 1, "Hi.", 1)
	if a := f.do(http.MethodPost, "/v1/sessions/"+bare.ID+"/fork", "alice", ``); a.status != http.StatusCreated {
		t.Fatalf("a fork of a session with no file: %d %s", a.status, a.body)
	}
}

// TestAFolderThatCannotBeOpened: a folder whose credential cannot be had
// refuses a message's file, a read and a fork as files_unavailable; a
// fork whose message's new file the files service refuses leaves no fork;
// and a folder the parent's no longer holds anything under is skipped,
// the fork going on (spec 068).
func TestAFolderThatCannotBeOpened(t *testing.T) {
	f := newFolderFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	f.turn(s.ID, 1, "Hi.", 1)
	report := f.keep(s.ID, 2, "results/2/report.pdf", "%PDF report")
	files := f.api.o.Files
	broken := func(context.Context, session.Session) (harness.Files, error) { return nil, errors.New("no token") }
	f.api.o.Files = &Files{Write: broken, Read: broken}
	if a, _ := f.attach(s.ID, "a.txt", "a"); a.code() != CodeFilesUnavailable {
		t.Fatalf("a message's file with no credential: %d %s", a.status, a.body)
	}
	if a := f.do(http.MethodGet, "/v1/sessions/"+s.ID+"/blobs/"+string(report), "alice", ""); a.code() != CodeFilesUnavailable {
		t.Fatalf("a read with no credential: %d %s", a.status, a.body)
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/fork", "alice", ``); a.code() != CodeFilesUnavailable {
		t.Fatalf("a fork with no credential: %d %s", a.status, a.body)
	}
	f.api.o.Files = files
	f.arca.Fail(arcastub.OpPut, arcastub.Failure{Status: http.StatusRequestEntityTooLarge, Code: "object_too_large"})
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/fork", "alice", `{"message":{"content":[],"attachments":[{"name":"big.bin","data":"`+b64("big")+`"}]}}`); a.code() != CodeAttachmentTooLarge {
		t.Fatalf("a fork whose new file is refused: %d %s", a.status, a.body)
	}
	f.arca.Remove(space, folderPath(s.ID)+"/results/2/report.pdf")
	a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/fork", "alice", ``)
	if a.status != http.StatusCreated {
		t.Fatalf("a fork of a folder emptied under it: %d %s", a.status, a.body)
	}
	var fork session.Session
	a.decode(t, &fork)
	if a := f.do(http.MethodGet, "/v1/sessions/"+fork.ID+"/blobs/"+string(report), "alice", ""); a.status != http.StatusNotFound {
		t.Fatalf("the fork's file its parent no longer held: %d", a.status)
	}
}

// keptIn are the ids of the files.kept events of the session id that
// record a file at ref.
func (ff *folderFixture) keptIn(id, ref string) []string {
	ff.t.Helper()
	evs, err := ff.sessions.Events(ff.t.Context(), id, 1, 0)
	if err != nil {
		ff.t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		for _, file := range e.Files() {
			if e.Type == session.TypeFilesKept && file.Ref == ref {
				out = append(out, e.ID)
			}
		}
	}
	return out
}

// redact redacts the event eid of the session id.
func (ff *folderFixture) redact(id, eid string) {
	ff.t.Helper()
	if a := ff.do(http.MethodPost, "/v1/sessions/"+id+"/events/"+eid+"/redact", "alice", `{}`); a.status != http.StatusNoContent {
		ff.t.Fatalf("redact %s: %d %s", eid, a.status, a.body)
	}
}

// TestARedactionDeletesItsFilesFromTheFolder: a file of the session's
// folder goes, for good and with the session's own token, once a
// redaction leaves no live event naming its ref: a message's attachment,
// and an answer's kept file once both answers of the turn that link it
// are redacted. A fork's copy is its own, so a redaction in its parent
// leaves it, and a fork's message that attaches its parent's file again
// keeps it while the copied message still names it. A delete the files
// service refuses leaves the redaction written and the file in place
// (spec 073).
func TestARedactionDeletesItsFilesFromTheFolder(t *testing.T) {
	f := newFolderFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	f.turn(s.ID, 1, "Hello.", 1)
	_, msg := f.attach(s.ID, "notes.txt", "my notes")
	var p session.UserMessage
	if err := msg.Decode(&p); err != nil {
		t.Fatal(err)
	}
	notes := folderPath(s.ID) + "/" + p.Attachments[0].Ref
	f.keep(s.ID, 2, "results/2/report.pdf", "%PDF report")
	f.keep(s.ID, 2, "results/2/report.pdf", "%PDF report")
	report := folderPath(s.ID) + "/results/2/report.pdf"
	answers := f.keptIn(s.ID, "results/2/report.pdf")
	if len(answers) != 2 {
		t.Fatalf("the turn's answers that link the report: %v", answers)
	}

	fork := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/fork", "alice",
		`{"message":{"content":[{"type":"text","text":"Again."}],"attachments":[{"name":"notes.txt","blob":"`+string(p.Attachments[0].Blob)+`"}]}}`)
	if fork.status != http.StatusCreated {
		t.Fatalf("fork: %d %s", fork.status, fork.body)
	}
	var child session.Session
	fork.decode(t, &child)
	childNotes := folderPath(child.ID) + "/" + p.Attachments[0].Ref

	f.redact(s.ID, answers[0])
	if _, ok := f.arca.Object(space, report); !ok {
		t.Fatal("a file another answer still links was deleted")
	}
	f.redact(s.ID, answers[1])
	if _, ok := f.arca.Object(space, report); ok {
		t.Fatal("a file no answer links any more is still in the folder")
	}
	f.redact(s.ID, msg.ID)
	if _, ok := f.arca.Object(space, notes); ok {
		t.Fatal("a redacted message's file is still in the folder")
	}
	for _, path := range []string{childNotes, folderPath(child.ID) + "/results/2/report.pdf"} {
		if _, ok := f.arca.Object(space, path); !ok {
			t.Fatalf("a redaction in the parent deleted the fork's %s", path)
		}
	}
	var deletes []arcastub.Recorded
	for _, r := range f.arca.Requests() {
		if r.Op == arcastub.OpDelete {
			deletes = append(deletes, r)
		}
	}
	if len(deletes) != 2 {
		t.Fatalf("the deletes asked %+v", deletes)
	}
	for _, r := range deletes {
		if r.Header.Get("Authorization") != "Bearer session:"+s.ID || !strings.HasPrefix(r.Path, "/v1/files/") {
			t.Fatalf("a delete carried %q at %s", r.Header.Get("Authorization"), r.Path)
		}
	}

	// The fork's own message names the parent's ref again, beside the
	// copied message that recorded it first.
	cevs, err := f.sessions.Events(t.Context(), child.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.redact(child.ID, cevs[len(cevs)-1].ID)
	if _, ok := f.arca.Object(space, childNotes); !ok {
		t.Fatal("a file the fork's copied message still names was deleted")
	}
	f.arca.Fail(arcastub.OpDelete, arcastub.Failure{Status: http.StatusForbidden, Code: "forbidden"})
	f.redact(child.ID, msg.ID)
	if _, ok := f.arca.Object(space, childNotes); !ok {
		t.Fatal("a refused delete removed the file")
	}
	redacted := false
	cevs, err = f.sessions.Events(t.Context(), child.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range cevs {
		redacted = redacted || e.ID == msg.ID && e.Redacted()
	}
	if !redacted {
		t.Fatal("a refused delete undid the redaction")
	}
}

// TestAForksRefsResolveInItsOwnFolder: every ref a fork's log records,
// copied from its parent or written by the fork's own message, opens in
// the fork's own folder and is answered by the fork's blob route, after
// the folder of the session it was forked from is emptied, as the
// installation's sweep empties a deleted conversation's: a fork at the
// last turn, a fork before a later message that attaches that message's
// file again by its digest, and a fork of a fork, whose own parent's
// folder is then emptied too (specs 068 and 073).
func TestAForksRefsResolveInItsOwnFolder(t *testing.T) {
	f := newFolderFixture(t)
	f.apply("alice", "reviewer", "Review.")
	parent := f.create("alice", "reviewer")
	f.turn(parent.ID, 1, "Hello.", 1)
	_, first := f.attach(parent.ID, "notes.txt", "my notes")
	f.keep(parent.ID, 2, "results/2/report.pdf", "%PDF report")
	_, later := f.attach(parent.ID, "later.txt", "a later file")
	f.keep(parent.ID, 3, "results/3/later.pdf", "%PDF later")
	blobOf := func(e session.Event) session.Digest {
		var m session.UserMessage
		if err := e.Decode(&m); err != nil {
			t.Fatal(err)
		}
		return m.Attachments[0].Blob
	}
	fork := func(id, body string) session.Session {
		a := f.do(http.MethodPost, "/v1/sessions/"+id+"/fork", "alice", body)
		if a.status != http.StatusCreated {
			t.Fatalf("fork of %s: %d %s", id, a.status, a.body)
		}
		var s session.Session
		a.decode(t, &s)
		return s
	}
	whole := fork(parent.ID, ``)
	before := fork(parent.ID, `{"before_seq":`+strconv.FormatUint(later.Seq, 10)+`,"message":{"content":[{"type":"text","text":"Instead."}],"attachments":[`+
		`{"name":"later.txt","blob":"`+string(blobOf(later))+`"},{"name":"notes.txt","blob":"`+string(blobOf(first))+`"}]}}`)
	grand := fork(whole.ID, ``)

	empty := func(id string) {
		for _, p := range f.arca.Paths(space, folderPath(id)+"/") {
			f.arca.Remove(space, p)
		}
	}
	resolves := func(s session.Session, want int) {
		t.Helper()
		store, err := filestore.New(*f.stored(s.ID).Folder, func(context.Context) (string, error) { return "session:" + s.ID, nil }, f.arca.Client())
		if err != nil {
			t.Fatal(err)
		}
		evs, err := f.sessions.Events(t.Context(), s.ID, 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range evs {
			for _, file := range e.Files() {
				n++
				rc, _, err := store.Open(t.Context(), file.Ref)
				if err != nil {
					t.Fatalf("%s's %s does not open in its own folder: %v", s.ID, file.Ref, err)
				}
				if err := rc.Close(); err != nil {
					t.Fatal(err)
				}
				if a := f.do(http.MethodGet, "/v1/sessions/"+s.ID+"/blobs/"+string(file.Digest), "alice", ""); a.status != http.StatusOK || session.DigestOf(a.body) != file.Digest {
					t.Fatalf("%s's blob route for %s: %d", s.ID, file.Ref, a.status)
				}
			}
		}
		if n != want {
			t.Fatalf("%s's log records %d files, want %d", s.ID, n, want)
		}
	}
	empty(parent.ID)
	resolves(whole, 4)
	resolves(before, 4)
	resolves(grand, 4)
	empty(whole.ID)
	resolves(grand, 4)
}

// save appends a turn whose end saved the files of written into the
// copy of the working directory, which the test writes into the folder
// first, and removed the paths of removed.
func (ff *folderFixture) save(id string, turn int, written map[string]string, removed ...string) {
	ff.t.Helper()
	s := ff.stored(id)
	w := session.WorkSaved{Covers: s.LastSeq, Removed: removed}
	for _, p := range slices.Sorted(maps.Keys(written)) {
		ff.arca.Put(space, folderPath(id)+"/work/"+p, arcastub.Object{Data: []byte(written[p])})
		w.Written = append(w.Written, session.WorkFile{Path: p, Size: int64(len(written[p])), Mode: 0o644, SHA256: session.DigestOf([]byte(written[p])).Hex()})
	}
	for _, p := range removed {
		ff.arca.Remove(space, folderPath(id)+"/work/"+p)
	}
	saved, err := session.NewEvent(session.TypeWorkSaved, w, time.Now())
	if err != nil {
		ff.t.Fatal(err)
	}
	idle, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopEndTurn}, time.Now())
	if err != nil {
		ff.t.Fatal(err)
	}
	batch := []session.Event{saved, idle}
	for i := range batch {
		batch[i].Turn = turn
	}
	session.Stamp(id, s.LastSeq, batch)
	if _, err := ff.sessions.Append(ff.t.Context(), id, s.LastSeq, batch); err != nil {
		ff.t.Fatal(err)
	}
}

// TestAForkCopiesItsParentsWorkingDirectory: a fork copies its parent's
// work/ into its own folder on the server, with its own token, and its log
// gains, after the copied events, a whole work.saved of the parent's copy
// as it stands, a later turn's included, whose covers is at most the fork
// point; a fork before the first message and a fork whose allow names no
// folder copy none; a parent whose
// work/ holds nothing is skipped; and a copy refused leaves no fork (spec
// 073).
func TestAForkCopiesItsParentsWorkingDirectory(t *testing.T) {
	f := newFolderFixture(t)
	f.apply("alice", "reviewer", "Review.")
	parent := f.create("alice", "reviewer")
	f.turn(parent.ID, 1, "Hello.", 1)
	f.save(parent.ID, 2, map[string]string{"src/app.js": "v1", "notes.md": "notes"})
	evs, err := f.sessions.Events(t.Context(), parent.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	at := evs[len(evs)-1].Seq
	f.save(parent.ID, 3, map[string]string{"src/app.js": "v2", "dist/index.html": "<h1>"}, "notes.md")

	a := f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/fork", "alice", `{"at_seq":`+strconv.FormatUint(at, 10)+`}`)
	if a.status != http.StatusCreated {
		t.Fatalf("fork: %d %s", a.status, a.body)
	}
	var fork session.Session
	a.decode(t, &fork)
	if got := f.arca.Paths(space, folderPath(fork.ID)+"/work/"); !slices.Equal(got, []string{folderPath(fork.ID) + "/work/dist/index.html", folderPath(fork.ID) + "/work/src/app.js"}) {
		t.Fatalf("the fork's work/ holds %v", got)
	}
	for _, r := range f.arca.Requests() {
		if r.Op == arcastub.OpCopy && strings.HasSuffix(r.Path, "/work") && r.Header.Get("Authorization") != "Bearer session:"+fork.ID {
			t.Fatalf("the copy of work carried %q", r.Header.Get("Authorization"))
		}
	}
	fevs, err := f.sessions.Events(t.Context(), fork.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	last := fevs[len(fevs)-1]
	var w session.WorkSaved
	if last.Type != session.TypeWorkSaved || last.Seq != at+1 || last.Decode(&w) != nil {
		t.Fatalf("the fork's log ends %s at %d", last.Type, last.Seq)
	}
	if !w.Whole || w.From != parent.ID || w.Covers != at || len(w.Written) != 2 || w.Written[1].SHA256 != session.DigestOf([]byte("v2")).Hex() || w.Held.Files != 2 {
		t.Fatalf("the fork's copy %+v", w)
	}
	if c := session.CopyOf(fevs); !c.Inherited || len(c.Files) != 2 || c.Covers != at {
		t.Fatalf("the fork's copy as it folds %+v", c)
	}
	// A fork before the first message continues nothing and takes no copy.
	if w, err := forkWork(&forkOrigin{parent: f.stored(parent.ID), seq: 0, work: session.CopyOf(evs)}, fork, time.Now()); w != nil || err != nil {
		t.Fatalf("a fork before the first message takes the copy %+v, %v", w, err)
	}

	// A fork with a message records its copy before the message.
	a = f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/fork", "alice", `{"message":{"content":[{"type":"text","text":"Again."}]}}`)
	if a.status != http.StatusCreated {
		t.Fatalf("fork with a message: %d %s", a.status, a.body)
	}
	var withMessage session.Session
	a.decode(t, &withMessage)
	mevs, err := f.sessions.Events(t.Context(), withMessage.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(mevs); mevs[n-1].Type != session.TypeUserMessage || !slices.ContainsFunc(mevs[len(evs)+2:], func(e session.Event) bool { return e.Type == session.TypeWorkSaved }) {
		t.Fatalf("the fork with a message ends %s", mevs[n-1].Type)
	}

	f.named = false
	a = f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/fork", "alice", ``)
	if a.status != http.StatusCreated {
		t.Fatalf("a fork with no folder: %d %s", a.status, a.body)
	}
	var bare session.Session
	a.decode(t, &bare)
	bevs, err := f.sessions.Events(t.Context(), bare.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range bevs[len(evs)+2:] {
		if e.Type == session.TypeWorkSaved {
			t.Fatal("a fork with no folder records a copy")
		}
	}
	f.named = true

	for _, p := range f.arca.Paths(space, folderPath(parent.ID)+"/work/") {
		f.arca.Remove(space, p)
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/fork", "alice", ``); a.status != http.StatusCreated {
		t.Fatalf("a fork of a parent whose work/ holds nothing: %d %s", a.status, a.body)
	}
	f.arca.Fail(arcastub.OpCopy, arcastub.Failure{Status: http.StatusRequestEntityTooLarge, Code: "quota_exceeded"})
	if a := f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/fork", "alice", ``); a.status != http.StatusServiceUnavailable || a.code() != CodeFilesUnavailable {
		t.Fatalf("a fork whose copy is refused: %d %s", a.status, a.body)
	}
}
