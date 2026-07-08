package state

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestStartEndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sess, err := NewSession(dir, 2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	in := &StartFile{
		SessionID:   sess.ID,
		StartedAt:   sess.StartedAt,
		TaskContext: "do the task",
		TaskSource:  "transcript",
		Proposer:    AgentRef{Agent: "claude", Model: "claude-sonnet-4-6"},
		Critic:      AgentRef{Agent: "codex"},
		Diff:        DiffSnap{From: "HEAD", To: ".", ChangedLines: 47, Files: []string{"a.py"}, PatchPath: "diff.patch"},
		Config: ConfigSnap{
			MaxTurn: 6, SideCount: 2,
			CostCap: 50000, ChangedLinesMin: 10, Format: "markdown",
		},
		RootSession:        RootSession{Cwd: "/tmp"},
		AdversarialVersion: "v0.0.1",
		GoVersion:          "go1.23",
	}
	if err := WriteStart(sess, in); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(sess.Path("start.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rt StartFile
	if err := json.Unmarshal(got, &rt); err != nil {
		t.Fatal(err)
	}
	if rt.Schema != SchemaStart {
		t.Errorf("schema: got %q", rt.Schema)
	}
	if rt.TaskContext != "do the task" {
		t.Errorf("task: got %q", rt.TaskContext)
	}
}

func TestAppendTranscript(t *testing.T) {
	dir := t.TempDir()
	sess, err := NewSession(dir, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		r := &TranscriptRecord{TS: time.Now(), Fork: 1, Round: i, Role: "critic", Path: "x", MS: 1}
		if err := AppendTranscript(sess, r); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(sess.Path("transcript.jsonl"))
	if n := strings.Count(string(b), "\n"); n != 3 {
		t.Errorf("lines: got %d, want 3", n)
	}
}

func TestAppendLog(t *testing.T) {
	dir := t.TempDir()
	if err := AppendLog(dir, &LogRecord{TS: time.Now(), Kind: "run", Session: "x", Termination: "steady-state"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dir + "/log.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"kind":"run"`) {
		t.Errorf("missing run record: %q", b)
	}
}

func TestWriteEnd(t *testing.T) {
	dir := t.TempDir()
	sess, err := NewSession(dir, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	end := &EndFile{
		SessionID:   sess.ID,
		EndedAt:     time.Now(),
		Termination: Termination{Reason: "steady-state"},
	}
	if err := WriteEnd(sess, end); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(sess.Path("end.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rt EndFile
	if err := json.Unmarshal(b, &rt); err != nil {
		t.Fatal(err)
	}
	if rt.Termination.Reason != "steady-state" {
		t.Errorf("termination: got %q", rt.Termination.Reason)
	}
	if rt.Schema != SchemaEnd {
		t.Errorf("schema: got %q", rt.Schema)
	}
}

func TestWriteRunDiff(t *testing.T) {
	dir := t.TempDir()
	sess, err := NewSession(dir, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRunDiff(sess, "diff content"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(sess.Path("diff.patch"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "diff content" {
		t.Errorf("got %q", b)
	}
}
