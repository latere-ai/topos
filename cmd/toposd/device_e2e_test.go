// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/client"
	cellav1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/test/stubs/luxstub"
)

// cellaListening is the line cellad prints once its listeners are up.
var cellaListening = regexp.MustCompile(`listening public=(\S+) internal=(\S+)`)

// process is a program the test started, its output kept.
type process struct {
	cmd  *exec.Cmd
	mu   sync.Mutex
	out  strings.Builder
	line chan string
}

// start runs bin with args and env, collecting its standard output and
// error, every line of which is also sent to line.
func start(t *testing.T, bin string, env []string, args ...string) *process {
	t.Helper()
	p := &process{cmd: exec.Command(bin, args...), line: make(chan string, 256)}
	p.cmd.Env = env
	r, w := io.Pipe()
	p.cmd.Stdout, p.cmd.Stderr = w, w
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			p.mu.Lock()
			p.out.WriteString(sc.Text() + "\n")
			p.mu.Unlock()
			select {
			case p.line <- sc.Text():
			default:
			}
		}
	}()
	t.Cleanup(func() {
		if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
			t.Logf("interrupt %s: %v", bin, err)
		}
		done := make(chan error, 1)
		go func() { done <- p.cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			if err := p.cmd.Process.Kill(); err != nil {
				t.Logf("kill %s: %v", bin, err)
			}
			<-done
		}
		if err := w.Close(); err != nil {
			t.Logf("close %s's output: %v", bin, err)
		}
	})
	return p
}

// output is what the process wrote so far.
func (p *process) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.out.String()
}

// grantedFolder is a folder the local driver can grant: one the person
// owns outside the system's directories, which on macOS rules out the
// temporary directory under /var.
func grantedFolder(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "darwin" {
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(home, ".topos-device-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}

// TestADeviceSessionEndToEnd is spec 080's conformance case: a hosted
// session whose machine is a device runs a command in a folder on the
// computer, through a real Cella control plane and a worker on the local
// driver, confined by the operating system's sandbox; its file is the
// folder's, and the session's end with its turn deletes the sandbox and
// leaves the folder. It needs a cellad binary of the Cella this module pins, named
// by CELLAD, and srt, the sandbox runtime, on PATH; without either it
// skips naming what is missing.
func TestADeviceSessionEndToEnd(t *testing.T) {
	cellad := os.Getenv("CELLAD")
	if cellad == "" {
		t.Skip("CELLAD names no cellad binary; build one with go install latere.ai/x/cella/cmd/cellad@<the version go.mod pins>")
	}
	srt, err := exec.LookPath("srt")
	switch {
	case err != nil:
		t.Skip("srt, the sandbox runtime Cella's local driver confines a computer's processes with, is not on PATH")
	case runtime.GOOS != "darwin" && runtime.GOOS != "linux":
		t.Skip("Cella's local driver runs on macOS and Linux alone")
	case os.Geteuid() == 0:
		t.Skip("Cella's local driver refuses to run as root")
	}
	ctx := t.Context()
	issuer := issuertest.New(t)
	admin := issuer.Mint(issuertest.Claims{Sub: "admin", Aud: issuertest.StringList{"cella"}})
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signing := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	path := filepath.Dir(srt) + ":" + os.Getenv("PATH")
	base := []string{"PATH=" + path, "HOME=" + os.Getenv("HOME")}

	plane := start(t, cellad, append(base,
		"CELLA_OIDC_ISSUERS="+issuer.URL(), "CELLA_ADMIN_SUBJECTS="+issuer.URL()+"|admin",
		"CELLA_RUNTIME=native", "CELLA_ALLOW_UNSAFE_NATIVE=true", "CELLA_TOKEN_KEY="+signing,
		"CELLA_PUBLIC_URL=http://127.0.0.1", "CELLA_DATA_DIR="+t.TempDir(),
		"CELLA_PUBLIC_ADDR=127.0.0.1:0", "CELLA_INTERNAL_ADDR=127.0.0.1:0"))
	var cellaURL string
	for deadline := time.After(30 * time.Second); cellaURL == ""; {
		select {
		case line := <-plane.line:
			if m := cellaListening.FindStringSubmatch(line); m != nil {
				cellaURL = "http://" + m[1]
			}
		case <-deadline:
			t.Fatalf("cellad never listened: %s", plane.output())
		}
	}
	c, err := client.New(client.Config{URL: cellaURL, Token: client.StaticToken(admin)})
	if err != nil {
		t.Fatal(err)
	}
	const env = "dev-e2e"
	doc, err := json.Marshal(cellav1.Environment{APIVersion: cellav1.APIVersion, Kind: cellav1.KindEnvironment, Metadata: cellav1.Metadata{Name: env},
		Spec: cellav1.EnvironmentSpec{Mode: cellav1.EnvironmentWorker, Isolation: cellav1.IsolationProcess,
			Capacity: cellav1.Capacity{CPU: "8", Memory: "16Gi", Disk: "100Gi", Sandboxes: 4}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.ApplyEnvironment(ctx, env, client.JSON(doc)); err != nil {
		t.Fatalf("apply the device's environment: %v", err)
	}
	minted, _, err := c.MintEnvironmentKey(ctx, env)
	if err != nil {
		t.Fatalf("mint the device's key: %v", err)
	}
	folder := grantedFolder(t)
	worker := start(t, cellad, append(base,
		"CELLA_URL="+cellaURL, "CELLA_ENVIRONMENT_KEY="+minted.Token, "CELLA_RUNTIME=local", "CELLA_DATA_DIR="+t.TempDir(),
		"CELLA_LOCAL_GRANTS="+folder+":rw", "CELLA_LOCAL_NETWORK=none"), "worker")
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		e, _, err := c.GetEnvironment(ctx, env)
		if err == nil && e.Status.Phase == cellav1.EnvironmentReady && len(e.Status.Hosts) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the device's environment never became ready: %+v, %v\nworker: %s", e.Status, err, worker.output())
		}
	}

	const model = "anthropic/claude-haiku-4.5"
	vars, lux, _ := hostedStubs(t,
		luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "toolu_1", Name: "bash", Args: json.RawMessage(`{"command":"echo from the device > hello.txt && pwd -P"}`)}}}, StopReason: ir.StopToolUse}},
		luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{{Type: ir.BlockText, Text: "Done."}}, StopReason: ir.StopEndTurn}})
	tokenFile := filepath.Join(t.TempDir(), "cella-token")
	if err := os.WriteFile(tokenFile, []byte(admin+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	maps.Copy(vars, map[string]string{"TOPOS_CELLA_URL": cellaURL, "TOPOS_CELLA_TOKEN_FILE": tokenFile,
		"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DATA_DIR": t.TempDir(), "TOPOS_RUNNER_CAPACITY": "1"})
	publicURL, _, stop := startServe(t, vars)
	send := apiClient(t, publicURL, vars)
	agent := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: builder\nspec:\n  model: {name: " + model + "}\n  tools: [bash]\n" +
		"  approvals: {mode: confirm, alwaysAllow: [bash]}\n"
	if code, body := send(http.MethodPut, "/v1/agents/builder", agent); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}
	code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"builder","message":"Say hello in a file.","end_on_idle":true,"machine":{"kind":"device","environment":"`+env+`","workdir":"`+folder+`"}}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var s struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(120 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		_, got := send(http.MethodGet, "/v1/sessions/"+s.ID, "")
		// The session ends with its turn, as it was created to, and its
		// runner deletes the sandbox then.
		if strings.Contains(got, `"status":"ended"`) || strings.Contains(got, `"status":"idle"`) && strings.Contains(got, `"stop_reason":`) {
			_, events := send(http.MethodGet, "/v1/sessions/"+s.ID+"/events", "")
			if !strings.Contains(got, `"stop_reason":"completed"`) {
				t.Fatalf("the device session stopped otherwise: %s\nevents %s\nworker: %s", got, events, worker.output())
			}
			for _, want := range []string{`"kind":"device"`, `"environment":"` + env + `"`, `"workdir":"` + folder + `"`, `"sandbox":"process"`} {
				if !strings.Contains(events, want) {
					t.Errorf("the log lacks %s: %s", want, events)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			_, events := send(http.MethodGet, "/v1/sessions/"+s.ID+"/events", "")
			t.Fatalf("the device session never answered: %s\nevents %s\nworker: %s", got, events, worker.output())
		}
	}
	if b, err := os.ReadFile(filepath.Join(folder, "hello.txt")); err != nil || string(b) != "from the device\n" {
		t.Fatalf("the folder holds %q, %v", b, err)
	}
	if n := len(lux.Requests()); n != 2 {
		t.Errorf("the model was asked %d times", n)
	}
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		list, _, err := c.ListSandboxes(context.WithoutCancel(ctx), client.ListOptions{})
		if err == nil && len(list) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session's end left %d sandboxes, %v", len(list), err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(folder, "hello.txt")); err != nil || string(b) != "from the device\n" {
		t.Fatalf("the session's end touched the folder: %q, %v", b, err)
	}
	if code := stop(); code != 0 {
		t.Fatalf("toposd exited %d", code)
	}
}
