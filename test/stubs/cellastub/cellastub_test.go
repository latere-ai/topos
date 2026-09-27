// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellastub

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/client"
	v1 "latere.ai/x/cella/manifest/v1"
)

func dial(t *testing.T, s *Server) *client.Client {
	t.Helper()
	c, err := client.New(client.Config{URL: s.URL(), Token: client.StaticToken("tok")})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func manifest(t *testing.T, name string, edit ...func(*v1.Sandbox)) client.Manifest {
	t.Helper()
	sb := v1.Sandbox{APIVersion: v1.APIVersion, Kind: v1.KindSandbox, Metadata: v1.Metadata{Name: name}}
	for _, e := range edit {
		e(&sb)
	}
	m, err := client.Encode(sb)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// create makes a running sandbox.
func create(t *testing.T, s *Server, name string, edit ...func(*v1.Sandbox)) (*client.Client, v1.Sandbox) {
	t.Helper()
	c := dial(t, s)
	sb, _, err := c.CreateSandbox(t.Context(), manifest(t, name, edit...), client.Wait(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return c, sb
}

func code(t *testing.T, err error, want string) {
	t.Helper()
	if got := client.CodeOf(err); got != want {
		t.Errorf("code %q (%v), want %q", got, err, want)
	}
}

func TestSandboxLifecycle(t *testing.T) {
	s := New(t)
	s.AddEnvironment("mine")
	s.AddSecret("gh", "api.github.com")
	c, sb := create(t, s, "ses-one", func(sb *v1.Sandbox) {
		sb.Metadata.Labels = map[string]string{"topos.latere.ai/session": "ses_1"}
		sb.Spec.Environment = "mine"
		sb.Spec.Secrets = []v1.SecretMount{{Name: "gh", Env: "GH_TOKEN"}}
	})
	if sb.Status.Phase != "Running" || sb.Status.Environment != "mine" || sb.Spec.Image != DefaultImage {
		t.Errorf("created = %+v", sb.Status)
	}
	ws := s.Workspace("ses-one")
	if ws == "" || sb.Spec.Workspace.Path != ws || sb.Spec.Workdir != ws {
		t.Errorf("workspace %q, spec %+v", ws, sb.Spec)
	}
	if fi, err := os.Stat(ws); err != nil || !fi.IsDir() {
		t.Errorf("the workspace is no directory: %v", err)
	}
	for _, ref := range []string{"ses-one", sb.Status.ID} {
		got, _, err := c.GetSandbox(t.Context(), ref)
		if err != nil || got.Status.ID != sb.Status.ID || got.Metadata.Labels["topos.latere.ai/session"] != "ses_1" {
			t.Errorf("get %s = %+v %v", ref, got.Metadata, err)
		}
	}
	if got, ok := s.Sandbox(sb.Status.ID); !ok || got.Metadata.Name != "ses-one" {
		t.Errorf("Sandbox = %+v %v", got.Metadata, ok)
	}

	_, _, err := c.CreateSandbox(t.Context(), manifest(t, "ses-one"))
	code(t, err, "name_taken")
	_, _, err = c.CreateSandbox(t.Context(), manifest(t, "Not_A_Label"))
	code(t, err, "invalid_field")
	_, _, err = c.CreateSandbox(t.Context(), manifest(t, "other", func(sb *v1.Sandbox) { sb.Spec.Environment = "nowhere" }))
	code(t, err, "not_found")
	_, _, err = c.CreateSandbox(t.Context(), manifest(t, "other", func(sb *v1.Sandbox) {
		sb.Spec.Secrets = []v1.SecretMount{{Name: "missing", Env: "X"}}
	}))
	code(t, err, "not_found")
	_, _, err = c.CreateSandbox(t.Context(), manifest(t, "other", func(sb *v1.Sandbox) { sb.Kind = "Secret" }))
	code(t, err, "invalid_field")
	_, _, err = c.CreateSandbox(t.Context(), client.JSON([]byte(`{"apiVersion":"x","surprise":1}`)))
	code(t, err, "unknown_field")

	_, _, err = c.StartSandbox(t.Context(), sb.Status.ID)
	code(t, err, "phase_conflict")
	stopped, _, err := c.StopSandbox(t.Context(), sb.Status.ID)
	if err != nil || stopped.Status.Phase != "Stopped" {
		t.Errorf("stop = %s %v", stopped.Status.Phase, err)
	}
	_, _, err = c.StopSandbox(t.Context(), sb.Status.ID)
	code(t, err, "phase_conflict")
	_, _, err = c.Exec(t.Context(), sb.Status.ID, client.ExecRequest{Command: []string{"true"}})
	code(t, err, "phase_conflict")
	s.StartAfter(2)
	started, _, err := c.StartSandbox(t.Context(), sb.Status.ID)
	if err != nil || started.Status.Phase != "Starting" {
		t.Errorf("start = %s %v", started.Status.Phase, err)
	}
	for _, want := range []string{"Starting", "Running"} {
		got, _, err := c.GetSandbox(t.Context(), sb.Status.ID)
		if err != nil || got.Status.Phase != want {
			t.Errorf("phase %s %v, want %s", got.Status.Phase, err, want)
		}
	}
	s.StartAfter(0)

	if !s.Stop("ses-one") {
		t.Error("Stop found no sandbox")
	}
	if got, _ := s.Sandbox("ses-one"); got.Status.Phase != "Stopped" || got.Status.Reason != "Idle" {
		t.Errorf("an idle stop = %+v", got.Status)
	}
	if !s.SetFailed("ses-one", "Exited") {
		t.Error("SetFailed found no sandbox")
	}
	deleted, err := c.Delete(t.Context(), client.KindSandbox, "ses-one")
	if err != nil || !bytes.Contains(deleted, []byte(`"Deleting"`)) {
		t.Errorf("delete = %s %v", deleted, err)
	}
	if _, err := os.Stat(ws); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the workspace outlived the sandbox: %v", err)
	}
	_, _, err = c.GetSandbox(t.Context(), "ses-one")
	code(t, err, "not_found")
	_, err = c.Delete(t.Context(), client.KindSandbox, "ses-one")
	code(t, err, "not_found")
	_, _, err = c.StartSandbox(t.Context(), "ses-one")
	code(t, err, "not_found")
	_, _, err = c.StopSandbox(t.Context(), "ses-one")
	code(t, err, "not_found")
	if s.Stop("ses-one") || s.Remove("ses-one") || s.Workspace("ses-one") != "" {
		t.Error("a deleted sandbox is still found")
	}
	if _, ok := s.Sandbox("ses-one"); ok {
		t.Error("a deleted sandbox is still found")
	}

	_, lost := create(t, s, "ses-two")
	if !s.Remove(lost.Status.ID) {
		t.Error("Remove found no sandbox")
	}
	_, _, err = c.GetSandbox(t.Context(), lost.Status.ID)
	code(t, err, "not_found")

	s.StartAfter(1)
	_, held := create(t, s, "ses-three")
	if held.Status.Phase != "Starting" {
		t.Errorf("a held create answers %s", held.Status.Phase)
	}
}

func TestTokenFailuresAndRecords(t *testing.T) {
	s := New(t)
	s.RequireToken("right")
	wrong, err := client.New(client.Config{URL: s.URL(), Token: client.StaticToken("wrong")})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = wrong.GetSandbox(t.Context(), "x")
	code(t, err, "unauthenticated")
	right, err := client.New(client.Config{URL: s.URL(), Token: client.StaticToken("right")})
	if err != nil {
		t.Fatal(err)
	}
	s.Fail(OpGet, Failure{Status: http.StatusServiceUnavailable, Code: "driver_unavailable", Times: 2})
	s.Fail(OpGet, Failure{Status: 418, Code: "teapot", Detail: "short and stout"})
	for _, want := range []string{"driver_unavailable", "driver_unavailable", "teapot", "not_found"} {
		_, _, err := right.GetSandbox(t.Context(), "x")
		code(t, err, want)
	}
	var ce *client.Error
	s.Fail(OpGet, Failure{Status: 418, Code: "teapot", Detail: "short and stout"})
	if _, _, err := right.GetSandbox(t.Context(), "x"); !errors.As(err, &ce) || ce.Message != "I'm a teapot" || ce.Detail != "short and stout" {
		t.Errorf("an injected refusal = %+v", ce)
	}
	resp, err := http.Get(s.URL() + "/v1/volumes")
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unserved route answers %d %v", resp.StatusCode, err)
	}
	reqs := s.Requests()
	if len(reqs) != 6 || reqs[0].Header.Get("Authorization") != "Bearer wrong" || reqs[0].Op != OpGet {
		t.Errorf("requests = %d, first %+v", len(reqs), reqs[0])
	}
	if n := s.Count(OpGet); n != 6 {
		t.Errorf("Count = %d", n)
	}
	s.record(nil)
	s.record(errors.New("recorded"))
	if errs := s.Errors(); len(errs) != 1 || errs[0].Error() != "recorded" {
		t.Errorf("Errors = %v", errs)
	}
}

func TestSecret(t *testing.T) {
	s := New(t)
	s.AddSecret("gh", "api.github.com", "github.com")
	c := dial(t, s)
	sec, _, err := c.GetSecret(t.Context(), "gh")
	if err != nil || strings.Join(sec.Spec.Scope.Hosts, ",") != "api.github.com,github.com" || sec.Spec.Value != "" {
		t.Errorf("secret = %+v %v", sec.Spec, err)
	}
	_, _, err = c.GetSecret(t.Context(), "gone")
	code(t, err, "not_found")
	apply := func(sec v1.Secret) (v1.Secret, error) {
		m, err := client.Encode(sec)
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := c.ApplySecret(t.Context(), sec.Metadata.Name, m)
		return got, err
	}
	key := v1.Secret{APIVersion: v1.APIVersion, Kind: v1.KindSecret, Metadata: v1.Metadata{Name: "key"},
		Spec: v1.SecretSpec{Kind: v1.SecretStatic, Scope: v1.SecretScope{Hosts: []string{"lux.example"}}, Value: "lux_one"}}
	got, err := apply(key)
	if err != nil || got.Spec.Value != "" || got.Status.Version != 1 {
		t.Fatalf("apply = %+v %v", got, err)
	}
	key.Spec.Value = "lux_two"
	if got, err := apply(key); err != nil || got.Status.Version != 2 || got.Status.ID != v1.SecretIDPrefix+"key" {
		t.Fatalf("a second apply = %+v %v", got, err)
	}
	if held, value, ok := s.Secret("key"); !ok || value != "lux_two" || held.Spec.Value != "" || held.Spec.Scope.Hosts[0] != "lux.example" {
		t.Fatalf("held %+v %q %v", held, value, ok)
	}
	key.Spec.Scope.Hosts = nil
	_, err = apply(key)
	code(t, err, "invalid_field")
	key.Metadata.Name = "other"
	if _, _, err := c.ApplySecret(t.Context(), "key", client.JSON([]byte(`{"kind":"Secret","metadata":{"name":"other"},"spec":{"scope":{"hosts":["h"]}}}`))); err == nil {
		t.Fatal("a secret applied under another name")
	}
	_, _, err = c.ApplySecret(t.Context(), "key", client.JSON([]byte(`{"nope":1}`)))
	code(t, err, "bad_request")
	if _, _, ok := s.Secret("none"); ok {
		t.Fatal("a secret nobody applied")
	}
}

func TestExecWait(t *testing.T) {
	s := New(t)
	s.AddSecret("gh", "api.github.com")
	c, sb := create(t, s, "ses-exec", func(sb *v1.Sandbox) {
		sb.Spec.Env = map[string]string{"FROM_SPEC": "spec"}
		sb.Spec.Secrets = []v1.SecretMount{{Name: "gh", Env: "GH_TOKEN"}}
	})
	if err := os.Mkdir(filepath.Join(sb.Spec.Workspace.Path, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, _, err := c.Exec(t.Context(), sb.Status.ID, client.ExecRequest{
		Command: []string{"sh", "-c", `pwd; echo "$FROM_SPEC $FROM_REQ $GH_TOKEN"; echo err >&2; exit 3`},
		Env:     map[string]string{"FROM_REQ": "req"},
		Workdir: filepath.Join(sb.Spec.Workspace.Path, "sub"),
	})
	want := filepath.Join(sb.Spec.Workspace.Path, "sub") + "\nspec req cella-placeholder-gh\n"
	if err != nil || res.ExitCode != 3 || res.Stdout != want || res.Stderr != "err\n" || res.Truncated {
		t.Errorf("exec = %+v %v", res, err)
	}
	res, _, err = c.Exec(t.Context(), sb.Status.ID, client.ExecRequest{Command: []string{"/bin/sh", "-c", "sleep 30"}, Timeout: "100ms"})
	if err != nil || res.ExitCode != 124 {
		t.Errorf("a timeout = %+v %v", res, err)
	}
	res, _, err = c.Exec(t.Context(), sb.Status.ID, client.ExecRequest{Command: []string{"/bin/sh", "-c", "head -c 1100000 /dev/zero"}})
	if err != nil || !res.Truncated || len(res.Stdout) != outputCap {
		t.Errorf("a long output = %d bytes, truncated %v, %v", len(res.Stdout), res.Truncated, err)
	}
	_, _, err = c.Exec(t.Context(), sb.Status.ID, client.ExecRequest{Command: []string{"/nonexistent/program"}})
	code(t, err, "driver_unavailable")
	for _, req := range []client.ExecRequest{{}, {Command: []string{"true"}, Timeout: "2h"}, {Command: []string{"true"}, Timeout: "soon"}} {
		_, _, err = c.Exec(t.Context(), sb.Status.ID, req)
		code(t, err, "invalid_field")
	}
	_, _, err = c.Exec(t.Context(), "gone", client.ExecRequest{Command: []string{"true"}})
	code(t, err, "not_found")
	for _, body := range []string{`{"command":["true"]}`, `{"command":["true"],"cols":80}`} {
		url := s.URL() + "/v1/sandboxes/" + sb.Status.ID + "/exec"
		if !strings.Contains(body, "cols") {
			url += "?stream=1"
		} else {
			url += "?wait=1"
		}
		resp, err := http.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil || resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d %v", body, resp.StatusCode, err)
		}
	}
}

func TestExecSession(t *testing.T) {
	s := New(t)
	c, sb := create(t, s, "ses-socket")
	sess, err := c.ExecSession(t.Context(), sb.Status.ID, client.ExecRequest{Command: []string{"/bin/sh", "-c", "head -c 5; echo; echo err >&2; exit 2"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Write([]byte("hello, and more")); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(sess)
	if err != nil {
		t.Fatal(err)
	}
	exit, err := sess.Wait()
	if err != nil || exit != 2 || string(out) != "hello\nerr\n" {
		t.Errorf("session = %q, exit %d, %v", out, exit, err)
	}
	if err := sess.Close(); err != nil {
		t.Errorf("close: %v", err)
	}

	sess, err = c.ExecSession(t.Context(), sb.Status.ID, client.ExecRequest{Command: []string{"sh", "-c", "sleep 30"}, Timeout: "100ms"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(sess); err != nil {
		t.Fatal(err)
	}
	if exit, err := sess.Wait(); err != nil || exit != 124 {
		t.Errorf("a timeout: exit %d %v", exit, err)
	}

	for req, want := range map[string]client.ExecRequest{
		"invalid_field":          {},
		"capability_unsupported": {Command: []string{"sh"}, Cols: 80, Rows: 24},
		"driver_unavailable":     {Command: []string{"/nonexistent/program"}},
	} {
		sess, err := c.ExecSession(t.Context(), sb.Status.ID, want)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(sess); client.CodeOf(err) != req {
			t.Errorf("%+v: %v, want %s", want, err, req)
		}
	}

	// A client that goes away ends the command.
	marker := filepath.Join(sb.Spec.Workspace.Path, "ended")
	sess, err = c.ExecSession(t.Context(), sb.Status.ID, client.ExecRequest{Command: []string{"/bin/sh", "-c", "echo up; cat; touch " + marker}})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if _, err := io.ReadFull(sess, buf); err != nil || string(buf) != "up\n" {
		t.Fatalf("read %q %v", buf, err)
	}
	if err := sess.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the command's input did not end with its session")
		}
		time.Sleep(20 * time.Millisecond)
	}

	_, err = c.ExecSession(t.Context(), "gone", client.ExecRequest{Command: []string{"true"}})
	code(t, err, "not_found")
	resp, err := http.Get(s.URL() + "/v1/sandboxes/" + sb.Status.ID + "/exec")
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a GET that is no upgrade answers %d %v", resp.StatusCode, err)
	}
}

func TestFirstFrameRules(t *testing.T) {
	s := New(t)
	_, sb := create(t, s, "ses-frames")
	for name, first := range map[string]func(*wsClient) error{
		"binary first":  func(w *wsClient) error { return w.send(opBinary, []byte("{}")) },
		"not json":      func(w *wsClient) error { return w.send(opText, []byte("{")) },
		"unknown field": func(w *wsClient) error { return w.send(opText, []byte(`{"command":["true"],"x":1}`)) },
	} {
		w := dialSocket(t, s, sb.Status.ID)
		if err := first(w); err != nil {
			t.Fatal(err)
		}
		op, payload, err := w.read()
		if err != nil || op != opClose || len(payload) < 2 || int(payload[0])<<8|int(payload[1]) != 1008 {
			t.Errorf("%s: %d %q %v, want a close 1008", name, op, payload, err)
		}
	}
	// A ping is answered, and a message may come in two frames.
	w := dialSocket(t, s, sb.Status.ID)
	if err := w.sendFrame(false, opText, []byte(`{"command":["/bin/sh","-c",`)); err != nil {
		t.Fatal(err)
	}
	if err := w.sendFrame(true, opContinuation, []byte(`"echo two"]}`)); err != nil {
		t.Fatal(err)
	}
	if err := w.send(opPing, []byte("p")); err != nil {
		t.Fatal(err)
	}
	pong, output, exit := false, "", ""
	for {
		op, payload, err := w.read()
		if err != nil {
			t.Fatal(err)
		}
		switch op {
		case opPong:
			pong = string(payload) == "p"
		case opBinary:
			output += string(payload)
		case opText:
			exit = string(payload)
		}
		if op == opClose {
			break
		}
	}
	if !pong || output != "two\n" || exit != `{"exit":0}` {
		t.Errorf("pong %v, output %q, exit %q", pong, output, exit)
	}
}

func TestExecSessionMessage(t *testing.T) {
	s := New(t)
	_, sb := create(t, s, "ses-message")
	w := dialSocket(t, s, sb.Status.ID)
	req, err := json.Marshal(execRequest{Command: []string{"/bin/sh", "-c", "cat"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.send(opText, req); err != nil {
		t.Fatal(err)
	}
	if err := w.send(opPong, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.send(0x3, nil); err != nil {
		t.Fatal(err)
	}
	// The unknown opcode ends the client's side; the command's input ends
	// with it and cat exits.
	for {
		op, _, err := w.read()
		if err != nil || op == opClose {
			break
		}
	}
}

func TestExecRules(t *testing.T) {
	s := New(t)
	c, sb := create(t, s, "ses-rules")
	for name, req := range map[string]client.ExecRequest{
		"a reserved variable": {Command: []string{"true"}, Env: map[string]string{"HTTPS_PROXY": "x"}},
		"a Cella variable":    {Command: []string{"true"}, Env: map[string]string{"CELLA_TOKEN": "x"}},
		"a bad name":          {Command: []string{"true"}, Env: map[string]string{"1X": "x"}},
		"a NUL":               {Command: []string{"true", "a\x00b"}},
		"a workdir outside":   {Command: []string{"true"}, Workdir: "/elsewhere"},
	} {
		_, _, err := c.Exec(t.Context(), sb.Status.ID, req)
		if client.CodeOf(err) != "invalid_field" {
			t.Errorf("%s: %v", name, err)
		}
		sess, err := c.ExecSession(t.Context(), sb.Status.ID, req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(sess); client.CodeOf(err) != "invalid_field" {
			t.Errorf("%s on the socket: %v", name, err)
		}
	}
	long := strings.Repeat("x", MaxBodyBytes)
	_, _, err := c.Exec(t.Context(), sb.Status.ID, client.ExecRequest{Command: []string{"echo", long}})
	code(t, err, "body_too_large")
	sess, err := c.ExecSession(t.Context(), sb.Status.ID, client.ExecRequest{Command: []string{"echo", long}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(sess); err == nil || !strings.Contains(err.Error(), "1009") {
		t.Errorf("a first frame past the body limit: %v", err)
	}
}
