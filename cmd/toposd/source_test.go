// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/arcastub"
	"latere.ai/x/topos/test/stubs/cellastub"
)

// TestAMessageBringsAFileOfThePersonsFiles: on an installation whose
// authorizer names a folder of a files store for each session, a message
// that names a file of the sender's own files by its source is read at
// the files API with the sender's own bearer, kept in the session's
// folder with the session's own token and recorded with its source, and
// the model's request carries the file's text after the line that names
// it, with no sandbox started for it (specs 068 and 079).
func TestAMessageBringsAFileOfThePersonsFiles(t *testing.T) {
	vars, s := credentialStubs(t, saidReply("The total is 15."))
	arca := arcastub.New(t)
	arca.RedirectAbove(16 << 20)
	const owner = "https://issuer.example|admin"
	s.az.SetRules(stub.Rule{Action: authorizer.ActionSessionCreate, Allow: true,
		Limits: map[string]any{"folder": map[string]any{"url": arca.URL(), "owner": owner, "path": "files/sessions/first"}}})
	vars["TOPOS_RUNNER_CAPACITY"] = "1"
	vars["TOPOS_FILES_AUDIENCE"] = "arca"
	publicURL, _, stop := startServe(t, vars)
	var tok bytes.Buffer
	if code := run(t.Context(), []string{"token"}, env(vars), &tok, io.Discard); code != 0 {
		t.Fatalf("token: exit %d", code)
	}
	bearer := strings.TrimSpace(tok.String())
	send := func(method, path, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, publicURL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(b)
	}
	manifest := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: reviewer\nspec:\n  model: {name: anthropic/claude-haiku-4.5}\n  machine: {kind: cella}\n"
	if code, body := send(http.MethodPut, "/v1/agents/reviewer", manifest); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}
	code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"reviewer"}`)
	var sess session.Session
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &sess) != nil {
		t.Fatalf("create: %d %s", code, body)
	}
	sales := "month,total\nJuly,7\nAugust,8\n"
	arca.Put("me", "files/Reports/q3.csv", arcastub.Object{Data: []byte(sales), ContentType: "text/csv"})
	code, body = send(http.MethodPost, "/v1/sessions/"+sess.ID+"/events", `{"type":"user.message","payload":{"content":[{"type":"text","text":"What is the total?"}],`+
		`"attachments":[{"name":"q3.csv","source":{"owner":"me","path":"files/Reports/q3.csv"}}]}}`)
	var sent session.Event
	if code != http.StatusOK || json.Unmarshal([]byte(body), &sent) != nil {
		t.Fatalf("send: %d %s", code, body)
	}
	waitAnswered(t, publicURL, vars, sess.ID)

	var p session.UserMessage
	if err := sent.Decode(&p); err != nil {
		t.Fatal(err)
	}
	path := "attachments/" + sent.ID + "/q3.csv"
	want := session.Attachment{Name: "q3.csv", MediaType: "text/csv", Size: int64(len(sales)), Blob: session.DigestOf([]byte(sales)), Path: path, Ref: path,
		Source: session.AttachmentSource{Owner: "me", Path: "files/Reports/q3.csv"}, Inline: true}
	if len(p.Attachments) != 1 || p.Attachments[0] != want {
		t.Fatalf("the record %+v, want %+v", p.Attachments, want)
	}
	o, ok := arca.Object(owner, "files/sessions/first/"+path)
	if !ok || string(o.Data) != sales {
		t.Fatalf("the session's folder holds %+v %t", o, ok)
	}
	if m, minted := s.idp.Token(o.Token); !minted || m.Workload != "session" || m.SessionID != sess.ID {
		t.Fatalf("the file was written with %+v", m)
	}
	var source int
	for _, r := range arca.Requests() {
		if r.Op == arcastub.OpRead && r.Path == "/v1/files/me/files/Reports/q3.csv" {
			source++
			if r.Header.Get("Authorization") != "Bearer "+bearer {
				t.Fatalf("the source was read with %q, not the sender's own bearer", r.Header.Get("Authorization"))
			}
		}
	}
	if source != 1 {
		t.Fatalf("the source was read %d times", source)
	}
	reqs := s.lux.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d model requests", len(reqs))
	}
	inline := "The text of " + path + ", so you need not read the file to see it:\n```\n" + strings.TrimRight(sales, "\n") + "\n```"
	var carried bool
	for _, m := range reqs[0].Request.Messages {
		for _, b := range m.Blocks {
			carried = carried || b.Text == inline
		}
	}
	if !carried {
		t.Fatalf("the request carried no text of the file: %+v", reqs[0].Request.Messages)
	}
	if n := s.cella.Count(cellastub.OpCreate); n != 0 {
		t.Fatalf("%d sandboxes were created for a turn that only read a file's text", n)
	}
	if code := stop(); code != 0 {
		t.Fatalf("serve exited %d", code)
	}
}

// TestASourceIsSpooledUnderTheDataDirectory: toposd spools a source past
// a short text under TOPOS_DATA_DIR while its send is checked, and removes
// the spool once the file is kept in the session's folder; its start
// removes the spool files a stopped toposd left there (spec 081).
func TestASourceIsSpooledUnderTheDataDirectory(t *testing.T) {
	vars, s := credentialStubs(t, saidReply("A scan."))
	arca := arcastub.New(t)
	arca.RedirectAbove(16 << 20)
	const owner = "https://issuer.example|admin"
	s.az.SetRules(stub.Rule{Action: authorizer.ActionSessionCreate, Allow: true,
		Limits: map[string]any{"folder": map[string]any{"url": arca.URL(), "owner": owner, "path": "files/sessions/scan"}}})
	vars["TOPOS_RUNNER_CAPACITY"] = "1"
	vars["TOPOS_FILES_AUDIENCE"] = "arca"
	spool := filepath.Join(vars["TOPOS_DATA_DIR"], "spool")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spool, "source-left"), []byte("a stopped toposd's"), 0o600); err != nil {
		t.Fatal(err)
	}
	publicURL, _, stop := startServe(t, vars)
	send := apiClient(t, publicURL, vars)
	left := func() []string {
		t.Helper()
		entries, err := os.ReadDir(spool)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return out
	}
	if l := left(); len(l) != 0 {
		t.Fatalf("the start left %v in the spool", l)
	}
	manifest := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: reviewer\nspec:\n  model: {name: anthropic/claude-haiku-4.5}\n  machine: {kind: cella}\n"
	if code, body := send(http.MethodPut, "/v1/agents/reviewer", manifest); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}
	code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"reviewer"}`)
	var sess session.Session
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &sess) != nil {
		t.Fatalf("create: %d %s", code, body)
	}
	scan := bytes.Repeat([]byte("%PDF-1.7 scan\n"), 150_000)
	arca.Put("me", "files/Scans/scan.pdf", arcastub.Object{Data: scan, ContentType: "application/pdf"})
	code, body = send(http.MethodPost, "/v1/sessions/"+sess.ID+"/events", `{"type":"user.message","payload":{"content":[{"type":"text","text":"What is this?"}],`+
		`"attachments":[{"name":"scan.pdf","source":{"owner":"me","path":"files/Scans/scan.pdf"}}]}}`)
	var sent session.Event
	if code != http.StatusOK || json.Unmarshal([]byte(body), &sent) != nil {
		t.Fatalf("send: %d %s", code, body)
	}
	var p session.UserMessage
	if err := sent.Decode(&p); err != nil {
		t.Fatal(err)
	}
	path := "attachments/" + sent.ID + "/scan.pdf"
	want := session.Attachment{Name: "scan.pdf", MediaType: "application/pdf", Size: int64(len(scan)), Blob: session.DigestOf(scan), Path: path, Ref: path,
		Source: session.AttachmentSource{Owner: "me", Path: "files/Scans/scan.pdf"}}
	if len(p.Attachments) != 1 || p.Attachments[0] != want {
		t.Fatalf("the record %+v, want %+v", p.Attachments, want)
	}
	if o, ok := arca.Object(owner, "files/sessions/scan/"+path); !ok || !bytes.Equal(o.Data, scan) {
		t.Fatalf("the session's folder holds %d bytes %t", len(o.Data), ok)
	}
	if l := left(); len(l) != 0 {
		t.Fatalf("the send left %v in the spool", l)
	}
	waitAnswered(t, publicURL, vars, sess.ID)
	if code := stop(); code != 0 {
		t.Fatalf("serve exited %d", code)
	}
}
