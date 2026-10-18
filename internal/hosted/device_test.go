// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/client"
	cellav1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/machine/cella"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/cellastub"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// laptop is the Cella environment a person's computer serves in these
// tests.
const laptop = "dev-01jaq7v3k9m2abcd"

// displayedAgent is the builder agent with a display name, whose calls
// its person allowed ahead, since a call on a person's computer asks in
// confirm otherwise.
const displayedAgent = `apiVersion: topos.latere.ai/v1
kind: Agent
metadata:
  name: builder
  displayName: Site builder
spec:
  model: {name: anthropic/claude-haiku-4.5}
  tools: [bash, read, write]
  approvals: {mode: confirm, alwaysAllow: [bash, read, write]}
  machine: {kind: cella}
`

// newDeviceCloud is a cloud whose session works in a folder on a
// person's computer, which the stub serves as laptop: an installation
// that mints every credential and names a git host and a connection
// service, so a Cella sandbox of it would mount their Secrets.
func newDeviceCloud(t *testing.T, mut func(*session.Session)) (*cloud, string) {
	t.Helper()
	folder, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	folder = filepath.ToSlash(folder)
	c := newCloudOf(t, displayedAgent, func(s *session.Session) {
		s.Machine = session.Machine{Kind: session.MachineDevice, Environment: laptop, Workdir: folder}
		s.ExpiresAt = time.Now().Add(time.Hour)
		if mut != nil {
			mut(s)
		}
	})
	c.cella.AddHostEnvironment(laptop, cellav1.HostInfo{OS: runtime.GOOS, Arch: runtime.GOARCH, Grants: []cellav1.HostGrant{{Path: folder, Write: true}},
		Network: cellav1.HostNetwork{Mode: "allowlist", Hosts: []string{"github.com"}}}, cellav1.IsolationProcess)
	c.o.Machines = ByKind(Cella(CellaOptions{URL: c.cella.URL(), Token: client.StaticToken("installation-bearer"), Helpers: helpers(t),
		ModelsURL: c.lux.URL(), OrigoURL: "https://origo.example", ConnectionsURL: "https://connections.example", ConnectionsAudience: "connections",
		ApprovalWait: 300 * time.Millisecond}), nil)
	c.creds = &issued{life: 15 * time.Minute}
	return c, folder
}

// created is the manifest the create of the session's sandbox carried.
func (c *cloud) created() cellav1.Sandbox {
	c.t.Helper()
	sb, ok := c.cella.Created(cella.SandboxName(c.s.ID))
	if !ok {
		c.t.Fatalf("no sandbox was created\nevents %s", c.dump())
	}
	return sb
}

// TestDeviceOpensWithNoSecrets: the Cella create of a device machine has
// workspace.host set to the session's folder on the session's
// environment, no image, no Secret mount, no LUX_URL and no variable of
// the runner's, and no Secret is made for it, on an installation whose
// Cella sandboxes mount the Lux, git host and connection Secrets; the
// session records the device with its environment, platform, isolation
// and folder (spec 080).
func TestDeviceOpensWithNoSecrets(t *testing.T) {
	c, folder := newDeviceCloud(t, nil)
	c.drive("List the folder.", bash("toolu_1", "ls"), said("Listed."))
	sb := c.created()
	if sb.Spec.Environment != laptop || sb.Spec.Workspace.Host != folder || sb.Spec.Image != "" || len(sb.Spec.Secrets) > 0 || len(sb.Spec.Env) > 0 {
		t.Fatalf("the device's create: %+v", sb.Spec)
	}
	if _, set := sb.Spec.Env[EnvLuxURL]; set {
		t.Fatalf("the device's create names LUX_URL: %v", sb.Spec.Env)
	}
	if n := c.cella.Count(cellastub.OpSecret); n != 0 {
		t.Fatalf("%d secret requests for a device", n)
	}
	var m session.SessionMachine
	if evs := c.events(session.TypeSessionMachine); len(evs) != 1 || evs[0].Decode(&m) != nil {
		t.Fatalf("session.machine: %s", c.dump())
	}
	want := session.AttachedMachine{Kind: session.MachineDevice, ID: m.Machine.ID, Workdir: folder, OS: runtime.GOOS, Arch: runtime.GOARCH, Environment: laptop,
		Sandbox: cellav1.IsolationProcess, Egress: m.Machine.Egress}
	if m.Machine != want || m.Reason != session.MachineAttached || !strings.Contains(m.Context, "Working directory: "+folder) {
		t.Fatalf("session.machine records %+v, want %+v", m, want)
	}
}

// TestDeviceLabels: a device session a trigger started labels its
// sandbox with the agent's version, the trigger and what it fires on,
// beside the session and the agent, and annotates the agent's display
// name; one a person started carries no trigger label (spec 080).
func TestDeviceLabels(t *testing.T) {
	fired, _ := newDeviceCloud(t, func(s *session.Session) {
		s.TriggerID, s.TriggerFiresOn = "trg_01JAQ7V3K9M2ABCDEFGHJKMNPQ", session.TriggerSchedule
	})
	fired.drive("Tidy up.", bash("toolu_1", "ls"), said("Tidied."))
	sb := fired.created()
	l := sb.Metadata.Labels
	if l[cella.LabelSession] != fired.s.ID || l[cella.LabelAgent] != "builder" || l[cella.LabelAgentVersion] != "1" ||
		l[cella.LabelTrigger] != "trg_01JAQ7V3K9M2ABCDEFGHJKMNPQ" || l[cella.LabelTriggerFiresOn] != session.TriggerSchedule {
		t.Fatalf("a trigger's device session labels its sandbox %v", l)
	}
	if a := sb.Metadata.Annotations[cella.AnnotationAgentName]; a != "Site builder" {
		t.Fatalf("the agent-name annotation is %q", a)
	}
	person, _ := newDeviceCloud(t, nil)
	person.drive("Tidy up.", bash("toolu_1", "ls"), said("Tidied."))
	pl := person.created().Metadata.Labels
	if _, set := pl[cella.LabelTrigger]; set || pl[cella.LabelTriggerFiresOn] != "" || pl[cella.LabelAgentVersion] != "1" {
		t.Fatalf("a person's device session labels its sandbox %v", pl)
	}
}

// TestDeviceGetsNoRepositoriesOrRestore: a device session that names a
// repository and carries a file delivers no repository into the folder,
// writes the file under the sandbox's scratch directory and not into the
// folder, keeps no checkpoint in the folder's repository, and once its
// sandbox is lost neither restores nor replaces anything: the next turn
// works in the same folder, its files as the person left them (spec
// 080).
func TestDeviceGetsNoRepositoriesOrRestore(t *testing.T) {
	c, folder := newDeviceCloud(t, func(s *session.Session) {
		s.Resources = []session.Resource{{Type: session.ResourceRepository, URL: "https://code.example/acme/site.git"}}
	})
	git := exec.Command("git", "init", "-q", folder)
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	ctx := t.Context()
	body := "a,b\n1,2\n"
	d, err := c.st.PutBlob(ctx, c.s.ID, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	msg, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: c.s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: "Add up the sheet."}}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	att := session.AttachmentPath(msg.ID, "sheet.csv")
	if msg, err = session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: c.s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: "Add up the sheet."}},
		Attachments: []session.Attachment{{Name: "sheet.csv", MediaType: "text/csv", Size: int64(len(body)), Blob: d, Path: att}}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	c.driveAfter(session.StopEndTurn, []session.Event{msg}, bash("toolu_1", `cat "$TMPDIR/topos/`+att+`" && echo kept > sum.txt`), said("Added."))
	if out := c.results(); len(out) != 1 || !strings.Contains(out[0], "1,2") {
		t.Fatalf("the command did not read the file under the scratch directory: %v\nevents %s", out, c.dump())
	}
	var m session.SessionMachine
	if evs := c.events(session.TypeSessionMachine); len(evs) != 1 || evs[0].Decode(&m) != nil || len(m.Repositories) != 0 {
		t.Fatalf("session.machine: %s", c.dump())
	}
	if _, err := os.Stat(filepath.Join(folder, session.AttachmentDir)); !os.IsNotExist(err) {
		t.Fatalf("the file reached the person's folder: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(folder, ".git", "info", "exclude")); err == nil && strings.Contains(string(b), session.AttachmentDir) {
		t.Fatalf("the folder's repository was told to exclude the attachments: %s", b)
	}
	if out, err := exec.Command("git", "-C", folder, "for-each-ref", "refs/topos").CombinedOutput(); err != nil || len(bytes.TrimSpace(out)) > 0 {
		t.Fatalf("the folder's repository holds the session's refs: %s %v", out, err)
	}
	for _, e := range c.events(session.TypeSessionStatus) {
		var p session.SessionStatus
		if e.Decode(&p) == nil && p.Checkpoint != nil {
			t.Fatalf("a device turn kept a checkpoint: %+v", p)
		}
	}
	for _, r := range c.cella.Requests() {
		if r.Query.Get("dest") != "" {
			t.Fatalf("an archive was written into the device: %s %s", r.Method, r.Query.Encode())
		}
	}

	// The sandbox is lost while the session is idle: the next turn makes
	// a new one on the same computer and in the same folder, restores
	// nothing into it and records no loss of files.
	if !c.cella.Remove(cella.SandboxName(c.s.ID)) {
		t.Fatal("no sandbox to lose")
	}
	c.drive("Show the sum.", bash("toolu_2", "cat sum.txt"), said("Shown."))
	if out := c.results(); len(out) != 2 || !strings.Contains(out[1], "kept") {
		t.Fatalf("the folder's file after the loss: %v", out)
	}
	if n := c.cella.Count(cellastub.OpCreate); n != 2 {
		t.Fatalf("%d creates", n)
	}
	for _, e := range c.events(session.TypeSessionMachine) {
		var p session.SessionMachine
		if e.Decode(&p) != nil || p.Reason == session.MachineReplaced || p.Reason == session.MachineRestored || p.Restore != nil || p.Checkpoint != nil {
			t.Fatalf("a device machine was restored or replaced: %+v", p)
		}
	}
	for _, e := range c.events(session.TypeSessionError) {
		var p session.SessionError
		if e.Decode(&p) == nil && p.Code == "machine_lost" {
			t.Fatalf("a device recorded a loss of files: %+v", p)
		}
	}
}

// resumed is a session.resumed of the session's initiator, as the resume
// route appends it for a session waiting for its machine.
func (c *cloud) resumed() []session.Event {
	c.t.Helper()
	s, err := c.st.Get(c.t.Context(), c.s.ID)
	if err != nil {
		c.t.Fatal(err)
	}
	e, err := session.NewEvent(session.TypeSessionResumed, session.SessionResumed{By: c.s.Initiator, Reason: "device_back", MaxCostUSDMicro: s.Budget.MaxCostUSDMicro}, time.Now())
	if err != nil {
		c.t.Fatal(err)
	}
	return []session.Event{e}
}

// stopped is the detail of the session's last session.status.
func (c *cloud) stopped() string {
	c.t.Helper()
	evs := c.events(session.TypeSessionStatus)
	var p session.SessionStatus
	if len(evs) == 0 || evs[len(evs)-1].Decode(&p) != nil {
		c.t.Fatalf("no status: %s", c.dump())
	}
	return p.Detail
}

// waited are the reasons of the session's session.machine_waiting, in
// order.
func (c *cloud) waited() []string {
	c.t.Helper()
	var out []string
	for _, e := range c.events(session.TypeMachineWaiting) {
		var p session.SessionMachineWaiting
		if err := e.Decode(&p); err != nil || p.Environment != laptop {
			c.t.Fatalf("session.machine_waiting %s: %v", e.Payload, err)
		}
		out = append(out, p.Reason)
	}
	return out
}

// lastResult is the text of the session's last tool result.
func (c *cloud) lastResult() string {
	c.t.Helper()
	evs := c.events(session.TypeToolResult)
	var p session.ToolResult
	if len(evs) == 0 || evs[len(evs)-1].Decode(&p) != nil || len(p.Content) == 0 {
		c.t.Fatalf("no result: %s", c.dump())
	}
	return p.Content[0].Text
}

// TestDeviceWaits: a turn whose computer is offline stops at once with
// machine_waiting, reason offline; a create the computer holds records
// session.machine_waiting approval while the call waits and stops the
// turn after the wait, leaving the create waiting on the computer; a
// computer whose owner refused records session.error machine_refused
// with its own words, answers that call and every later one of the turn
// without asking again, and the turn goes on; a computer lost under a
// command stops the turn as offline; and a resume, or the next message,
// continues the session on the running sandbox (spec 080).
func TestDeviceWaits(t *testing.T) {
	t.Run("offline", func(t *testing.T) {
		c, _ := newDeviceCloud(t, nil)
		c.cella.SetEnvironmentPhase(laptop, cellav1.EnvironmentOffline)
		c.driveAfter(session.StopMachineWaiting, []session.Event{c.message("Build it.")}, bash("toolu_1", "make"))
		if got := c.waited(); len(got) != 1 || got[0] != session.MachineOffline || c.stopped() != session.MachineOffline {
			t.Fatalf("waited %v with detail %q\nevents %s", got, c.stopped(), c.dump())
		}
		if r := c.lastResult(); r != prompts.Render(prompts.CallMachineWaiting, prompts.Data{"Approval": false}) {
			t.Fatalf("the call's result: %q", r)
		}
		if n := len(c.lux.Requests()); n != 1 {
			t.Fatalf("%d model requests: the turn went on without the computer", n)
		}
		c.cella.SetEnvironmentPhase(laptop, cellav1.EnvironmentReady)
		c.driveAfter(session.StopEndTurn, c.resumed(), bash("toolu_2", "echo built"), said("Built."))
		if r := c.lastResult(); !strings.Contains(r, "built") {
			t.Fatalf("the resumed turn's call: %q", r)
		}
	})
	t.Run("approval", func(t *testing.T) {
		c, _ := newDeviceCloud(t, nil)
		c.cella.HoldCreates(laptop, true)
		c.driveAfter(session.StopMachineWaiting, []session.Event{c.message("Build it.")}, bash("toolu_1", "make"))
		if got := c.waited(); len(got) != 1 || got[0] != session.MachineApproval || c.stopped() != session.MachineApproval {
			t.Fatalf("waited %v with detail %q\nevents %s", got, c.stopped(), c.dump())
		}
		waiting, result := c.events(session.TypeMachineWaiting)[0], c.events(session.TypeToolResult)[0]
		if waiting.Seq > result.Seq {
			t.Fatalf("session.machine_waiting %d follows the call's result %d: it was not recorded while the call waited", waiting.Seq, result.Seq)
		}
		name := cella.SandboxName(c.s.ID)
		if sb, ok := c.cella.Sandbox(name); !ok || sb.Status.Phase != "Pending" {
			t.Fatalf("the create no longer waits on the computer: %+v", sb.Status)
		}
		c.cella.Admit(name)
		c.driveAfter(session.StopEndTurn, c.resumed(), bash("toolu_2", "echo built"), said("Built."))
		if n := c.cella.Count(cellastub.OpCreate); n != 1 || !strings.Contains(c.lastResult(), "built") {
			t.Fatalf("%d creates; the resumed call: %q", n, c.lastResult())
		}
	})
	t.Run("refused", func(t *testing.T) {
		c, _ := newDeviceCloud(t, nil)
		c.cella.HoldCreates(laptop, true)
		name := cella.SandboxName(c.s.ID)
		go func() {
			for range 1000 {
				if sb, ok := c.cella.Sandbox(name); ok && sb.Status.Phase == "Pending" {
					c.cella.RefuseCreate(name, "not while I am presenting")
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
		c.driveAfter(session.StopEndTurn, []session.Event{c.message("Build it.")}, bash("toolu_1", "make"), bash("toolu_2", "make test"), said("Your computer did not allow it."))
		refusal := prompts.Render(prompts.CallMachineRefused, prompts.Data{"Detail": "not while I am presenting"})
		for _, e := range c.events(session.TypeToolResult) {
			var p session.ToolResult
			if e.Decode(&p) != nil || p.Content[0].Text != refusal {
				t.Fatalf("a call on the refused computer: %s", e.Payload)
			}
		}
		errs := c.events(session.TypeSessionError)
		var p session.SessionError
		if len(errs) != 1 || errs[0].Decode(&p) != nil || p.Code != "machine_refused" || p.Message != harness.MessageMachineRefused || p.Detail != "not while I am presenting" {
			t.Fatalf("session.error: %s", c.dump())
		}
		if n := c.cella.Count(cellastub.OpCreate); n != 1 {
			t.Fatalf("%d creates: the turn asked the computer again", n)
		}
		// The next turn asks again, and the owner allows it.
		c.cella.HoldCreates(laptop, false)
		c.drive("Try again.", bash("toolu_3", "echo built"), said("Built."))
		if n := c.cella.Count(cellastub.OpCreate); n != 2 || !strings.Contains(c.lastResult(), "built") {
			t.Fatalf("%d creates; the next turn's call: %q", n, c.lastResult())
		}
	})
	t.Run("lost under a command", func(t *testing.T) {
		c, _ := newDeviceCloud(t, nil)
		c.drive("Look.", bash("toolu_0", "ls"), said("Looked."))
		long := bash("toolu_1", "sleep 10")
		long.Respond = func(*ir.Request, *ir.Response) {
			go func() {
				time.Sleep(500 * time.Millisecond)
				c.cella.Disconnect(laptop)
			}()
		}
		c.driveAfter(session.StopMachineWaiting, []session.Event{c.message("Build it.")}, long)
		if got := c.waited(); len(got) != 1 || got[0] != session.MachineOffline || c.stopped() != session.MachineOffline {
			t.Fatalf("waited %v with detail %q\nevents %s", got, c.stopped(), c.dump())
		}
		if r := c.lastResult(); r != prompts.Text(prompts.CallMachineAway) {
			t.Fatalf("the lost call's result: %q", r)
		}
		c.cella.Reconnect(laptop)
		c.drive("Go on.", bash("toolu_2", "echo back"), said("Back."))
		if n := c.cella.Count(cellastub.OpCreate); n != 1 || !strings.Contains(c.lastResult(), "back") {
			t.Fatalf("%d creates; the next message's call: %q", n, c.lastResult())
		}
	})
}

// calls is one response that makes each call, a tool name and its
// arguments by id, in order.
func calls(uses ...ir.ToolUse) luxstub.Reply {
	blocks := make([]ir.Block, 0, len(uses))
	for i := range uses {
		blocks = append(blocks, ir.Block{Type: ir.BlockToolUse, ToolUse: &uses[i]})
	}
	return luxstub.Reply{Response: ir.Response{Model: haiku, Blocks: blocks, StopReason: ir.StopToolUse}}
}

// TestADeviceAsksItsOwnerOnceATurn: calls of one step that meet a
// computer's refusal together, parallel reads among them, are each
// answered by the one refusal, recorded once, and ask the owner no second
// time; calls of one step that meet a held create wait for the owner
// once, record one session.machine_waiting, and stop the turn; and a
// message that arrived while a turn lost the computer is answered in the
// same drive once the computer is back, on the sandbox it had (spec 080).
func TestADeviceAsksItsOwnerOnceATurn(t *testing.T) {
	read := func(id, p string) ir.ToolUse {
		return ir.ToolUse{ID: id, Name: "read", Args: json.RawMessage(`{"path":"` + p + `"}`)}
	}
	run := func(id, command string) ir.ToolUse {
		args, err := json.Marshal(map[string]string{"command": command})
		if err != nil {
			panic(err)
		}
		return ir.ToolUse{ID: id, Name: "bash", Args: args}
	}
	t.Run("refused in parallel", func(t *testing.T) {
		c, _ := newDeviceCloud(t, nil)
		c.cella.HoldCreates(laptop, true)
		name := cella.SandboxName(c.s.ID)
		go func() {
			for range 1000 {
				if sb, ok := c.cella.Sandbox(name); ok && sb.Status.Phase == "Pending" {
					c.cella.RefuseCreate(name, "not now")
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
		c.driveAfter(session.StopEndTurn, []session.Event{c.message("Read both.")}, calls(read("toolu_1", "a.md"), read("toolu_2", "b.md")), said("It said no."))
		refusal := prompts.Render(prompts.CallMachineRefused, prompts.Data{"Detail": "not now"})
		results := c.events(session.TypeToolResult)
		for _, e := range results {
			var p session.ToolResult
			if e.Decode(&p) != nil || p.Content[0].Text != refusal {
				t.Fatalf("a parallel call on the refused computer: %s", e.Payload)
			}
		}
		if n, m, w := c.cella.Count(cellastub.OpCreate), len(c.events(session.TypeSessionError)), len(c.events(session.TypeMachineWaiting)); len(results) != 2 || n != 1 || m != 1 || w > 1 {
			t.Fatalf("%d results, %d creates, %d errors, %d waits\nevents %s", len(results), n, m, w, c.dump())
		}
	})
	t.Run("held for a step of calls", func(t *testing.T) {
		c, _ := newDeviceCloud(t, nil)
		c.cella.HoldCreates(laptop, true)
		began := time.Now()
		c.driveAfter(session.StopMachineWaiting, []session.Event{c.message("Build it.")}, calls(run("toolu_1", "make"), run("toolu_2", "make test"), run("toolu_3", "make lint")))
		if got := c.waited(); len(got) != 1 || got[0] != session.MachineApproval {
			t.Fatalf("waited %v\nevents %s", got, c.dump())
		}
		if took := time.Since(began); took > 2*300*time.Millisecond+time.Second {
			t.Fatalf("the step waited %s, more than one wait for the owner", took)
		}
		if n, r := c.cella.Count(cellastub.OpCreate), len(c.events(session.TypeToolResult)); n != 1 || r != 3 {
			t.Fatalf("%d creates and %d results", n, r)
		}
	})
	t.Run("a message while the computer was away", func(t *testing.T) {
		c, _ := newDeviceCloud(t, nil)
		c.drive("Look.", bash("toolu_0", "ls"), said("Looked."))
		long := bash("toolu_1", "sleep 10")
		long.Respond = func(*ir.Request, *ir.Response) {
			go func() {
				time.Sleep(500 * time.Millisecond)
				c.cella.Disconnect(laptop)
				e, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: c.s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: "Are you there?"}}}, time.Now())
				if err != nil {
					t.Error(err)
					return
				}
				for {
					s, err := c.st.Get(t.Context(), c.s.ID)
					if err != nil {
						t.Error(err)
						return
					}
					batch := []session.Event{e}
					session.Stamp(s.ID, s.LastSeq, batch)
					if _, err := c.st.Append(t.Context(), s.ID, s.LastSeq, batch); err == nil {
						return
					}
				}
			}()
		}
		back := bash("toolu_2", "echo back")
		back.Respond = func(*ir.Request, *ir.Response) { c.cella.Reconnect(laptop) }
		c.driveAfter(session.StopEndTurn, []session.Event{c.message("Build it.")}, long, back, said("Back."))
		if n := c.cella.Count(cellastub.OpCreate); n != 1 || !strings.Contains(c.lastResult(), "back") {
			t.Fatalf("%d creates; the next turn's call: %q\nevents %s", n, c.lastResult(), c.dump())
		}
		if got := c.waited(); len(got) != 1 || got[0] != session.MachineOffline {
			t.Fatalf("waited %v", got)
		}
	})
}
