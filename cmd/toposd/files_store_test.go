// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/internal/config"
	"latere.ai/x/topos/internal/credentials"
	idp "latere.ai/x/topos/internal/identity"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/arcastub"
)

// TestASessionsFilesLiveInTheFilesStore: on an installation whose
// authorizer names a folder of a files store for each session, a message
// with an attachment and an answer that links a file it wrote leave both
// in the session's folder, written with the session's own token; the
// runner delivers the attachment into the sandbox from there; the session
// answers its folder; the blob route answers both by digest, read with
// toposd's own token through the files API's redirect, as a stream with
// its length; the turn keeps the working directory under work/; and a
// fork copies them and the working directory's copy into its own folder
// with its own token and reads them from there (specs 068 and 073).
func TestASessionsFilesLiveInTheFilesStore(t *testing.T) {
	vars, s := credentialStubs(t,
		bashReply("toolu_1", "mkdir -p out && printf '%%PDF-1.7 report' > out/report.pdf && cat attachments/*/notes.txt"),
		saidReply("The notes are read. [The report](out/report.pdf)"),
	)
	arca := arcastub.New(t)
	const owner = "https://issuer.example|admin"
	folder := func(path string) map[string]any {
		return map[string]any{"folder": map[string]any{"url": arca.URL(), "owner": owner, "path": path}}
	}
	s.az.SetRules(
		stub.Rule{Action: authorizer.ActionSessionCreate, Allow: true, Limits: folder("files/sessions/first")},
		stub.Rule{Action: authorizer.ActionSessionFork, Allow: true, Limits: folder("files/sessions/fork")},
	)
	vars["TOPOS_RUNNER_CAPACITY"] = "1"
	vars["TOPOS_FILES_AUDIENCE"] = "arca"
	publicURL, _, stop := startServe(t, vars)
	send := apiClient(t, publicURL, vars)
	manifest := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: reviewer\nspec:\n  model: {name: anthropic/claude-haiku-4.5}\n  machine: {kind: cella}\n"
	if code, body := send(http.MethodPut, "/v1/agents/reviewer", manifest); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}
	code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"reviewer"}`)
	var sess session.Session
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &sess) != nil || sess.Folder != nil {
		t.Fatalf("create: %d %s", code, body)
	}
	notes := "Remember the tide tables."
	code, body = send(http.MethodPost, "/v1/sessions/"+sess.ID+"/events", `{"type":"user.message","payload":{"content":[{"type":"text","text":"Read my notes and write the report."}],`+
		`"attachments":[{"name":"notes.txt","media_type":"text/plain","data":"`+base64.StdEncoding.EncodeToString([]byte(notes))+`"}]}}`)
	var sent session.Event
	if code != http.StatusOK || json.Unmarshal([]byte(body), &sent) != nil {
		t.Fatalf("send: %d %s", code, body)
	}
	waitAnswered(t, publicURL, vars, sess.ID)

	attachment := "files/sessions/first/attachments/" + sent.ID + "/notes.txt"
	report := "files/sessions/first/results/1/out/report.pdf"
	for path, want := range map[string]string{attachment: notes, report: "%PDF-1.7 report"} {
		o, ok := arca.Object(owner, path)
		if !ok || string(o.Data) != want {
			t.Fatalf("%s holds %q, %t; the folder holds %v", path, o.Data, ok, arca.Paths(owner, "files/"))
		}
		m, minted := s.idp.Token(o.Token)
		if !minted || m.Audience != "arca" || m.Workload != "session" || m.SessionID != sess.ID {
			t.Fatalf("%s was written with %+v", path, m)
		}
	}
	_, events := send(http.MethodGet, "/v1/sessions/"+sess.ID+"/events", "")
	if !strings.Contains(events, notes) {
		t.Fatalf("the sandbox was not given the attachment: %s", events)
	}
	var page struct {
		Items []session.Event `json:"items"`
	}
	if err := json.Unmarshal([]byte(events), &page); err != nil {
		t.Fatal(err)
	}
	var kept session.KeptFile
	for _, e := range page.Items {
		var p session.FilesKept
		if e.Type == session.TypeFilesKept && e.Decode(&p) == nil && len(p.Files) == 1 {
			kept = p.Files[0]
		}
	}
	if kept.Ref != "results/1/out/report.pdf" || kept.Blob != session.DigestOf([]byte("%PDF-1.7 report")) {
		t.Fatalf("the kept file %+v", kept)
	}
	code, body = send(http.MethodGet, "/v1/sessions/"+sess.ID, "")
	if code != http.StatusOK || !strings.Contains(body, `"folder":{"owner":"`+owner+`","path":"files/sessions/first"}`) || strings.Contains(body, arca.URL()) {
		t.Fatalf("the session answers %d %s", code, body)
	}

	blobs := map[session.Digest]string{session.DigestOf([]byte(notes)): notes, kept.Blob: "%PDF-1.7 report"}
	var tok bytes.Buffer
	if code := run(t.Context(), []string{"token"}, env(vars), &tok, io.Discard); code != 0 {
		t.Fatalf("token: exit %d", code)
	}
	read := func(id string) {
		t.Helper()
		for d, want := range blobs {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, publicURL+"/v1/sessions/"+id+"/blobs/"+string(d), nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(tok.String()))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(resp.Body)
			if err = errors.Join(err, resp.Body.Close()); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK || string(b) != want || resp.ContentLength != int64(len(want)) || resp.Header.Get("ETag") != strconv.Quote(string(d)) {
				t.Fatalf("the blob %s of %s: %d %v %q", d, id, resp.StatusCode, resp.Header, b)
			}
		}
	}
	// The runner delivered the attachment with the session's own token;
	// the blob route's reads are toposd's own.
	before := len(arca.Requests())
	var delivered int
	for _, r := range arca.Requests() {
		if r.Op != arcastub.OpRead {
			continue
		}
		delivered++
		if m, minted := s.idp.Token(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")); !minted || m.Audience != "arca" || m.Workload != "session" || m.SessionID != sess.ID {
			t.Fatalf("the runner read the attachment with %+v", m)
		}
	}
	if delivered != 1 {
		t.Fatalf("the runner read the folder %d times", delivered)
	}
	read(sess.ID)
	var reads int
	for _, r := range arca.Requests()[before:] {
		if r.Op != arcastub.OpRead {
			continue
		}
		reads++
		if aud, ok := s.idp.ServiceToken(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")); !ok || aud != "arca" {
			t.Fatalf("a read carried a token that is not toposd's own for arca: %q", r.Header.Get("Authorization"))
		}
	}
	if reads != len(blobs) {
		t.Fatalf("%d reads of the files API", reads)
	}

	code, body = send(http.MethodPost, "/v1/sessions/"+sess.ID+"/fork", ``)
	var fork session.Session
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &fork) != nil || fork.Folder == nil || fork.Folder.Path != "files/sessions/fork" {
		t.Fatalf("fork: %d %s", code, body)
	}
	var copies []string
	for _, r := range arca.Requests() {
		if r.Op != arcastub.OpCopy {
			continue
		}
		m, minted := s.idp.Token(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if !minted || m.Audience != "arca" || m.SessionID != fork.ID || m.Workload != "session" {
			t.Fatalf("a copy carried %+v", m)
		}
		copies = append(copies, r.Path[strings.Index(r.Path, "/files/sessions/first/")+len("/files/sessions/first/"):])
	}
	slices.Sort(copies)
	if !slices.Equal(copies, []string{"attachments/" + sent.ID, "results/1", "work"}) {
		t.Fatalf("the fork copied %v", copies)
	}
	// The session's turn kept its working directory under work/, the file
	// its command made and the attachment delivered into it, and the fork
	// holds the same copy (spec 073).
	work := []string{"work/attachments/" + sent.ID + "/notes.txt", "work/out/report.pdf"}
	var forkWork, firstWork []string
	for _, p := range work {
		firstWork, forkWork = append(firstWork, "files/sessions/first/"+p), append(forkWork, "files/sessions/fork/"+p)
	}
	if got := arca.Paths(owner, "files/sessions/first/work/"); !slices.Equal(got, firstWork) {
		t.Fatalf("the session's work/ holds %v", got)
	}
	want := append([]string{"files/sessions/fork/attachments/" + sent.ID + "/notes.txt", "files/sessions/fork/results/1/out/report.pdf"}, forkWork...)
	if got := arca.Paths(owner, "files/sessions/fork/"); !slices.Equal(got, want) {
		t.Fatalf("the fork's folder holds %v", got)
	}
	arca.Remove(owner, attachment)
	arca.Remove(owner, report)
	read(fork.ID)
	if code := stop(); code != 0 {
		t.Fatalf("serve exited %d", code)
	}
}

// TestTheAPIOpensAFolderOnlyWhereItIsConfigured: the API keeps no files
// store without TOPOS_FILES_AUDIENCE or without the identity provider
// that mints its tokens, and with both it opens no folder for a session
// that names none (spec 068).
func TestTheAPIOpensAFolderOnlyWhereItIsConfigured(t *testing.T) {
	minter, err := credentials.New(credentials.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ic, err := idp.New(idp.Options{URL: "https://auth.example", ClientID: "topos-host", SecretFile: "/run/secret", HTTP: http.DefaultClient})
	if err != nil {
		t.Fatal(err)
	}
	if f := files(config.Config{}, minter, ic); f != nil {
		t.Fatal("a files store with no audience")
	}
	if f := files(config.Config{FilesAudience: "arca"}, nil, nil); f != nil {
		t.Fatal("a files store with no identity provider")
	}
	f := files(config.Config{FilesAudience: "arca"}, minter, ic)
	if f == nil {
		t.Fatal("no files store")
	}
	for name, open := range map[string]func(context.Context, session.Session) (harness.Files, error){"write": f.Write, "read": f.Read} {
		if _, err := open(t.Context(), session.Session{ID: "ses_x"}); err == nil {
			t.Errorf("%s opened a folder for a session with none", name)
		}
	}
}
