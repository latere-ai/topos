// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/dir"
)

var update = flag.Bool("update", false, "rewrite the golden logs and transcripts under testdata/fold")

const sessionID = "ses_01J9Z3P9D2F6H8K0M2Q4S6T8W0"

var t0 = time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

// logb builds a log with deterministic ids and times.
type logb struct {
	t      *testing.T
	events []session.Event
}

func (b *logb) id(n int) string { return fmt.Sprintf("evt_01J9Z3Q4W8KX6T0M2V5N7R%04d", n) }

// add appends an event of thread and turn and returns its id.
func (b *logb) add(typ session.Type, thread string, turn int, p any) string {
	b.t.Helper()
	n := len(b.events) + 1
	raw, err := session.Marshal(p)
	if err != nil {
		b.t.Fatal(err)
	}
	e := session.Event{
		ID: b.id(n), Seq: uint64(n), SessionID: sessionID, Type: typ,
		Time: t0.Add(time.Duration(n) * time.Second), Turn: turn, Thread: thread, Payload: raw,
	}
	b.events = append(b.events, e)
	return e.ID
}

// thread starts a thread whose id is its thread.started event's id.
func (b *logb) thread(parent, task string, turn int) string {
	n := len(b.events) + 1
	id := b.id(n)
	b.add(session.TypeThreadStarted, id, turn, session.ThreadStarted{
		Agent:  session.AgentRef{ID: "agent_01J9Z3Q4W8KX6T0M2V5N7R0000", Name: "reviewer", Version: 1},
		Parent: parent, Task: task, Depth: 1,
	})
	return id
}

func (b *logb) redact(id string) {
	i := slices.IndexFunc(b.events, func(e session.Event) bool { return e.ID == id })
	b.events[i].Payload = json.RawMessage(`{"tombstone":true}`)
	b.add(session.TypeEventRedacted, "", 0, session.EventRedacted{EventID: id, By: ada})
}

var (
	ada   = session.Sender{Subject: "usr_ada", Name: "Ada", Kind: session.SenderPerson}
	grace = session.Sender{Subject: "usr_grace", Name: "Grace", Kind: session.SenderPerson}
)

func txt(s string) lux.Block { return lux.Block{Type: ir.BlockText, Text: s} }

func use(id, name string) lux.Block {
	return lux.Block{Type: ir.BlockToolUse, ToolUse: &lux.ToolUse{ID: id, Name: name, Args: json.RawMessage(`{"command":"ls"}`)}}
}

func (b *logb) user(s session.Sender, text string, turn int) string {
	return b.add(session.TypeUserMessage, "", turn, session.UserMessage{Sender: s, Content: []lux.Block{txt(text)}})
}

func (b *logb) assistant(thread string, turn int, truncated bool, blocks ...lux.Block) string {
	stop := ir.StopEndTurn
	if truncated {
		stop = ir.StopMaxTokens
	}
	return b.add(session.TypeAgentMessage, thread, turn, session.AgentMessage{
		Message: lux.Message{Role: ir.RoleAssistant, Blocks: blocks}, StopReason: stop, Truncated: truncated,
	})
}

func (b *logb) toolUse(thread string, turn int, id string) {
	b.add(session.TypeAgentToolUse, thread, turn, session.AgentToolUse{ToolUseID: id, Name: "bash", Input: json.RawMessage(`{"command":"ls"}`), Verdict: "allow"})
}

func (b *logb) result(thread string, turn int, id, out string) {
	b.add(session.TypeToolResult, thread, turn, session.ToolResult{ToolUseID: id, Content: []lux.Block{txt(out)}, Outcome: "ok"})
}

// foldCases are the golden logs of spec 004's fold rules. Each names the
// threads it folds.
var foldCases = []struct {
	name    string
	build   func(b *logb) []string
	wantErr error
}{
	{"conversation", func(b *logb) []string {
		b.user(ada, "Fix the failing test.", 1)
		b.add(session.TypeModelRequest, "", 1, session.ModelRequest{Model: "m", Outcome: "ok"})
		b.assistant("", 1, false, lux.Block{Type: ir.BlockThinking, Text: "look first", Signature: "sig"}, txt("Done."))
		b.add(session.TypeSessionStatus, "", 1, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopEndTurn})
		return []string{""}
	}, nil},
	{"opaque_reasoning", func(b *logb) []string {
		// A Responses reasoning item travels as an opaque block, its raw
		// JSON in the escaped form encoding/json writes, and folds back
		// byte for byte beside the thinking it was summarized as.
		b.user(ada, "Plan the migration.", 1)
		b.assistant("", 1, false,
			lux.Block{Type: ir.BlockThinking, Text: "compare the two schemas"},
			lux.Block{Type: ir.BlockOpaque, Opaque: &lux.Opaque{Dialect: ir.DialectOpenAIResponses, Kind: "reasoning",
				Raw: json.RawMessage(`{"encrypted_content":"gAAA\u003cb\u0026c\u003e","id":"rs_1","type":"reasoning"}`)}},
			txt("Two steps."))
		b.user(ada, "Go on.", 2)
		return []string{""}
	}, nil},
	{"tool_results_in_tool_use_order", func(b *logb) []string {
		b.user(ada, "Run three things.", 1)
		b.assistant("", 1, false, txt("Running."), use("toolu_a", "bash"), use("toolu_b", "bash"), use("toolu_c", "bash"))
		for _, id := range []string{"toolu_a", "toolu_b", "toolu_c"} {
			b.toolUse("", 1, id)
		}
		b.result("", 1, "toolu_c", "c")
		b.user(ada, "Also check the docs.", 1)
		b.result("", 1, "toolu_a", "a")
		b.add(session.TypeUserToolResult, "", 1, session.UserToolResult{Sender: ada, ToolUseID: "toolu_b", Content: []lux.Block{txt("b")}})
		b.result("", 1, "toolu_orphan", "no tool_use names me")
		b.assistant("", 1, false, txt("All three ran."))
		return []string{""}
	}, nil},
	{"truncated_message", func(b *logb) []string {
		b.user(ada, "Write the file.", 1)
		b.assistant("", 1, true, txt("Writing"), use("toolu_kept", "bash"), use("toolu_partial", "write"))
		b.toolUse("", 1, "toolu_kept")
		b.result("", 1, "toolu_kept", "ok")
		b.assistant("", 1, false, txt("continued"))
		return []string{""}
	}, nil},
	{"open_tool_uses", func(b *logb) []string {
		b.user(ada, "Go.", 1)
		b.assistant("", 1, false, use("toolu_1", "bash"))
		b.toolUse("", 1, "toolu_1")
		b.result("", 1, "toolu_1", "one")
		b.assistant("", 1, false, use("toolu_2", "bash"), use("toolu_3", "bash"))
		b.toolUse("", 1, "toolu_2")
		b.toolUse("", 1, "toolu_3")
		b.result("", 1, "toolu_3", "three")
		return []string{""}
	}, nil},
	{"summary_replaces_its_range", func(b *logb) []string {
		b.user(ada, "first", 1)
		b.assistant("", 1, false, txt("reply one"))
		b.user(ada, "second", 2)
		b.assistant("", 2, false, txt("reply two"))
		b.user(ada, "third", 3)
		b.add(session.TypeContextCompacted, "", 3, session.ContextCompacted{Kind: session.CompactSummary, FromSeq: 1, ToSeq: 2, Summary: "Ada asked once.", Cause: session.CauseThreshold})
		b.add(session.TypeContextCompacted, "", 3, session.ContextCompacted{Kind: session.CompactSummary, FromSeq: 2, ToSeq: 4, Summary: "Ada asked twice.", Cause: session.CauseThreshold})
		b.assistant("", 3, false, txt("reply three"))
		return []string{""}
	}, nil},
	{"summary_of_other_threads_events", func(b *logb) []string {
		b.user(ada, "start", 1)
		th := b.thread("", "Review the diff.", 1)
		b.assistant(th, 1, false, txt("reviewing"))
		b.add(session.TypeContextCompacted, "", 1, session.ContextCompacted{Kind: session.CompactSummary, FromSeq: 2, ToSeq: 3, Summary: "A reviewer ran."})
		b.user(ada, "next", 2)
		return []string{""}
	}, nil},
	{"cleared_tool_results", func(b *logb) []string {
		b.user(ada, "List.", 1)
		b.assistant("", 1, false, use("toolu_big", "bash"), use("toolu_small", "bash"))
		b.toolUse("", 1, "toolu_big")
		b.toolUse("", 1, "toolu_small")
		b.result("", 1, "toolu_big", strings.Repeat("line\n", 20))
		b.result("", 1, "toolu_small", "short")
		b.add(session.TypeContextCompacted, "", 2, session.ContextCompacted{Kind: session.CompactClearToolResults, ToolUseIDs: []string{"toolu_big"}, Cause: session.CauseThreshold})
		b.user(ada, "Again.", 2)
		return []string{""}
	}, nil},
	{"senders_and_interrupts", func(b *logb) []string {
		b.user(ada, "Plan the release.", 1)
		b.assistant("", 1, false, txt("Planning."))
		b.add(session.TypeUserToolConfirmation, "", 1, session.UserToolConfirmation{Sender: ada, ToolUseID: "toolu_x", Decision: session.DecisionAllow})
		b.user(grace, "Skip the changelog.", 2)
		b.add(session.TypeUserInterrupt, "", 2, session.UserInterrupt{Sender: grace})
		b.user(ada, "Keep it.", 3)
		return []string{""}
	}, nil},
	{"threads_and_messages", func(b *logb) []string {
		b.user(ada, "Review then fix.", 1)
		b.assistant("", 1, false, use("toolu_spawn", "spawn"))
		b.toolUse("", 1, "toolu_spawn")
		th := b.thread("", "Review the diff in main.go.", 1)
		b.result("", 1, "toolu_spawn", "started "+b.id(4))
		b.assistant(th, 1, false, txt("One issue on line 3."))
		b.add(session.TypeThreadMessage, "", 1, session.ThreadMessage{From: th, FromName: "reviewer", Content: []lux.Block{txt("Line 3 leaks a file.")}})
		b.add(session.TypeThreadMessage, th, 1, session.ThreadMessage{To: th, FromName: "builder", Content: []lux.Block{txt("Check line 9 too.")}})
		b.add(session.TypeThreadEnded, th, 1, session.ThreadEnded{Reason: "completed", FinalText: "done"})
		return []string{"", th}
	}, nil},
	{"system_parts", func(b *logb) []string {
		b.add(session.TypeSessionMachine, "", 0, session.SessionMachine{Machine: session.AttachedMachine{Kind: "host", Workdir: "/old"}, Reason: "attached", Context: "old context"})
		b.add(session.TypeMemoryAttached, "", 0, session.MemoryAttached{MemoryStoreID: "mem_01J9Z3Q4W8KX6T0M2V5N7R0001", Name: "notes", Access: "read_write", Path: "/mnt/memory/notes", Version: "1"})
		b.add(session.TypeMemoryAttached, "", 0, session.MemoryAttached{MemoryStoreID: "mem_01J9Z3Q4W8KX6T0M2V5N7R0002", Name: "team", Access: "read_only", Path: "/mnt/memory/team"})
		b.user(ada, "Hi.", 1)
		b.add(session.TypeSessionMachine, "", 1, session.SessionMachine{
			Machine: session.AttachedMachine{Kind: "cella", Workdir: "/work"}, Reason: "handoff", Context: "Working directory /work on linux/arm64.",
			Instructions: []session.Instructions{{Path: "AGENTS.md", SHA256: "abc", Blob: session.Digest("sha256:" + strings.Repeat("a", 64))}},
			Skills:       []session.Skill{{Name: "release", Description: "Cut a release.", Path: ".agents/skills/release/SKILL.md"}},
		})
		b.add(session.TypeMemoryAttached, "", 1, session.MemoryAttached{MemoryStoreID: "mem_01J9Z3Q4W8KX6T0M2V5N7R0001", Name: "notes", Access: "read_write", Path: "/mnt/memory/notes", Version: "2"})
		m := b.add(session.TypeMemoryAttached, "", 1, session.MemoryAttached{MemoryStoreID: "mem_01J9Z3Q4W8KX6T0M2V5N7R0003", Name: "secret", Access: "read_only", Path: "/mnt/memory/secret"})
		b.redact(m)
		return []string{""}
	}, nil},
	{"rewound", func(b *logb) []string {
		b.user(ada, "Try the refactor.", 1)
		b.assistant("", 1, false, txt("Refactored."))
		b.add(session.TypeSessionRewound, "", 2, session.SessionRewound{ToTurn: 0, Checkpoint: session.CheckpointRef{Ref: "refs/topos/checkpoints/s/0", Commit: "c0"}, By: ada})
		b.user(ada, "Try something smaller.", 2)
		return []string{""}
	}, nil},
	{"refused_connections", func(b *logb) []string {
		// A command's refused connections read after its result; the
		// person's allow reads as reachable once the network was widened
		// for it, an allow it could not take as still out of reach, and a
		// deny as a host not to try again (spec 052).
		b.user(ada, "Install the dependencies.", 1)
		b.assistant("", 1, false, use("toolu_1", "bash"))
		b.toolUse("", 1, "toolu_1")
		b.result("", 1, "toolu_1", "curl: (56) CONNECT tunnel failed, response 403")
		b.add(session.TypeApprovalRequested, "", 1, session.ApprovalRequested{ApprovalID: "apr_1", ToolUseID: "toolu_1", Source: session.ApprovalFromEgress,
			Destination: session.Destination{Host: "registry.example.com", Port: 443}, Reason: session.ReasonConnectionOutside, Verdict: "ask"})
		b.add(session.TypeApprovalRequested, "", 1, session.ApprovalRequested{ApprovalID: "apr_2", ToolUseID: "toolu_1", Source: session.ApprovalFromEgress,
			Destination: session.Destination{Host: "cdn.example.com", Port: 443}, Reason: session.ReasonConnectionOutside, Verdict: "ask", More: 1})
		b.add(session.TypeApprovalRequested, "", 1, session.ApprovalRequested{ApprovalID: "apr_3", ToolUseID: "toolu_1", Source: session.ApprovalFromEgress,
			Destination: session.Destination{Host: "mirror.example.org", Port: 443}, Reason: session.ReasonConnectionOutside, Verdict: "ask"})
		b.add(session.TypeSessionStatus, "", 1, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopToolConfirmation})
		b.add(session.TypeUserToolConfirmation, "", 0, session.UserToolConfirmation{Sender: ada, ApprovalID: "apr_1", Decision: session.DecisionAllow})
		b.add(session.TypeUserToolConfirmation, "", 0, session.UserToolConfirmation{Sender: ada, ApprovalID: "apr_2", Decision: session.DecisionAllow})
		b.add(session.TypeUserToolConfirmation, "", 0, session.UserToolConfirmation{Sender: ada, ApprovalID: "apr_3", Decision: session.DecisionDeny, Note: "Use the registry."})
		b.add(session.TypeNetworkChanged, "", 2, session.NetworkChanged{Added: []string{"registry.example.com"}, Source: session.NetworkFromPerson, ApprovalID: "apr_1"})
		b.add(session.TypeApprovalDecided, "", 2, session.ApprovalDecided{ApprovalID: "apr_1", Decision: session.DecisionAllow, By: ada})
		b.add(session.TypeSessionError, "", 2, session.SessionError{Code: "network_unavailable", Message: "The session's network could not be widened."})
		b.add(session.TypeApprovalDecided, "", 2, session.ApprovalDecided{ApprovalID: "apr_2", Decision: session.DecisionAllow, By: ada})
		b.add(session.TypeApprovalDecided, "", 2, session.ApprovalDecided{ApprovalID: "apr_3", Decision: session.DecisionDeny, By: ada, Note: "Use the registry."})
		return []string{""}
	}, nil},
	{"redacted_and_summarized", func(b *logb) []string {
		secret := b.user(ada, "my token is abc", 1)
		b.assistant("", 1, false, txt("Noted."))
		b.redact(secret)
		b.add(session.TypeContextCompacted, "", 2, session.ContextCompacted{Kind: session.CompactSummary, FromSeq: 1, ToSeq: 2, Summary: "Ada shared a credential, since removed.", Cause: session.CauseRedaction})
		b.user(ada, "Continue.", 2)
		return []string{""}
	}, nil},
	{"redacted_uncompacted", func(b *logb) []string {
		secret := b.user(ada, "my token is abc", 1)
		b.assistant("", 1, false, txt("Noted."))
		b.redact(secret)
		return []string{""}
	}, session.ErrRedactionUncompacted},
	{"unknown_type", func(b *logb) []string {
		b.user(ada, "Hi.", 1)
		b.add("future.kind", "", 1, map[string]int{"n": 1})
		b.add("future.kind", "", 1, map[string]int{"n": 2})
		b.add("other.kind", "", 1, map[string]int{})
		return []string{""}
	}, nil},
}

func foldAll(t *testing.T, events []session.Event, threads []string) ([]byte, error) {
	t.Helper()
	out := map[string]session.Transcript{}
	for _, th := range threads {
		tr, err := session.Fold(events, th)
		if err != nil {
			return nil, err
		}
		out[th] = tr
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n'), nil
}

func writeLog(t *testing.T, path string, events []session.Event) {
	t.Helper()
	var buf bytes.Buffer
	for _, e := range events {
		b, err := session.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readLog(t *testing.T, path string) []session.Event {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []session.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var e session.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFoldRendersEveryType(t *testing.T) {
	for _, c := range foldCases {
		t.Run(c.name, func(t *testing.T) {
			b := &logb{t: t}
			threads := c.build(b)
			base := filepath.Join("testdata", "fold", c.name)
			got, err := foldAll(t, b.events, threads)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("fold: %v, want %v", err, c.wantErr)
				}
				got = []byte(fmt.Sprintf("error: %v\n", err))
			} else if err != nil {
				t.Fatal(err)
			}
			if *update {
				writeLog(t, base+".jsonl", b.events)
				if err := os.WriteFile(base+".golden", got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(base + ".golden")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("fold of %s differs from %s.golden:\n%s", c.name, base, got)
			}
			if logged := readLog(t, base+".jsonl"); !slices.EqualFunc(logged, b.events, session.SameEvent) {
				t.Fatalf("%s.jsonl is not the log the case builds; run with -update", base)
			}
		})
	}
}

// TestFoldIsByteIdenticalAcrossStores stores every golden log in the
// in-memory store and the directory store, reads it back, and folds it
// twice: every fold is the golden bytes.
func TestFoldIsByteIdenticalAcrossStores(t *testing.T) {
	ctx := t.Context()
	d, err := dir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stores := map[string]session.Store{"memory": session.NewMemoryStore(), "dir": d}
	for _, c := range foldCases {
		if c.wantErr != nil {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			base := filepath.Join("testdata", "fold", c.name)
			events := readLog(t, base+".jsonl")
			threads := c.build(&logb{t: t})
			want, err := os.ReadFile(base + ".golden")
			if err != nil {
				t.Fatal(err)
			}
			for name, st := range stores {
				s := session.New(session.AgentRef{ID: "agent_01J9Z3Q4W8KX6T0M2V5N7R0000", Version: 1}, ada, session.RunnerExternal, session.Machine{Kind: session.MachineHost}, t0)
				s.ID = session.NewID(session.PrefixSession)
				if err := st.Create(ctx, s, nil); err != nil {
					t.Fatal(err)
				}
				batch := slices.Clone(events)
				for i := range batch {
					batch[i].SessionID = s.ID
				}
				if _, err := st.Append(ctx, s.ID, 0, batch); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				stored, err := st.Events(ctx, s.ID, 1, 0)
				if err != nil {
					t.Fatal(err)
				}
				for i := range stored {
					stored[i].SessionID = sessionID
				}
				for run := range 2 {
					got, err := foldAll(t, stored, threads)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(got, want) {
						t.Fatalf("%s store, run %d: fold differs from the golden transcript", name, run)
					}
				}
			}
		})
	}
}

func TestFoldIsPure(t *testing.T) {
	b := &logb{t: t}
	b.user(ada, "Hi.", 1)
	b.assistant("", 1, false, txt("Hello."))
	shuffled := []session.Event{b.events[1], b.events[0]}
	a, err := session.Fold(b.events, "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := session.Fold(shuffled, "")
	if err != nil {
		t.Fatal(err)
	}
	x, _ := session.Marshal(a)
	y, _ := session.Marshal(c)
	if !bytes.Equal(x, y) {
		t.Fatal("the fold depends on the order it is given events in")
	}
	if shuffled[0].Seq != 2 {
		t.Fatal("the fold reordered its input")
	}
}

func TestFoldRejectsMalformed(t *testing.T) {
	for name, build := range map[string]func(b *logb){
		"summary past itself": func(b *logb) {
			b.user(ada, "x", 1)
			b.add(session.TypeContextCompacted, "", 1, session.ContextCompacted{Kind: session.CompactSummary, FromSeq: 1, ToSeq: 5})
		},
		"undecodable payload": func(b *logb) {
			b.user(ada, "x", 1)
			b.events[0].Payload = json.RawMessage(`{"content":"not blocks"}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := &logb{t: t}
			build(b)
			if _, err := session.Fold(b.events, ""); err == nil {
				t.Fatal("folded a malformed log")
			}
		})
	}
}
