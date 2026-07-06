package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"latere.ai/x/topos/adversarial/internal/critic"
)

func TestNewCriticPanicsOnUnknown(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	NewCritic("unknown")
}

func TestNewCriticReturnsCorrectImpl(t *testing.T) {
	if _, ok := NewCritic("codex").(*CodexCritic); !ok {
		t.Error("codex factory: wrong type")
	}
	if _, ok := NewCritic("claude").(*ClaudeCritic); !ok {
		t.Error("claude factory: wrong type")
	}
}

func TestAssemblePrompt(t *testing.T) {
	a := critic.Lookup("security")
	in := CriticInput{
		Aspect: a, CriticIndex: 1, Round: 1, SystemPrompt: "SYS",
		TaskContext: "TASK", DiffPatch: "DIFF",
		PriorRoundFiles: []RoundFileRef{{Path: "rounds/r2-proposer.md", Round: 2, Role: "proposer"}},
	}
	got := AssemblePrompt(in)
	if !strings.Contains(got, "SYS") || !strings.Contains(got, "# Task") || !strings.Contains(got, "DIFF") {
		t.Errorf("missing block; got: %.300s", got)
	}
	if !strings.Contains(got, "@rounds/r2-proposer.md") {
		t.Error("missing prior round reference")
	}
}

// Smoke test: when bin is missing, Exec returns an error and we map it.
func TestCodexMissingBinary(t *testing.T) {
	c := &CodexCritic{Bin: "/no/such/binary-z9z"}
	_, err := c.Round(context.Background(), CriticInput{
		Aspect: critic.Lookup("security"), CriticIndex: 1, Round: 1,
		SystemPrompt: "SYS", TaskContext: "T", DiffPatch: "D",
		Cwd: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestClaudeCriticMissingBinary(t *testing.T) {
	c := &ClaudeCritic{Bin: "/no/such/binary-z9z"}
	_, err := c.Round(context.Background(), CriticInput{
		Aspect: critic.Lookup("security"), CriticIndex: 1, Round: 1,
		SystemPrompt: "SYS", TaskContext: "T", DiffPatch: "D",
		Cwd: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCodexCritic_RateLimit(t *testing.T) {
	dir := t.TempDir()
	stub := dir + "/codex"
	if err := os.WriteFile(stub, []byte("#!/usr/bin/env bash\necho 'rate limit exceeded' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &CodexCritic{Bin: stub}
	_, err := c.Round(context.Background(), CriticInput{
		Aspect: critic.Lookup("security"), CriticIndex: 1, Round: 1,
		SystemPrompt: "SYS", TaskContext: "T", DiffPatch: "D",
		Cwd: t.TempDir(),
	})
	if !errors.Is(err, ErrRateLimit) {
		t.Errorf("got %v, want ErrRateLimit", err)
	}
}

func TestCodexCritic_TTYRequired(t *testing.T) {
	dir := t.TempDir()
	stub := dir + "/codex"
	if err := os.WriteFile(stub, []byte("#!/usr/bin/env bash\necho 'stdin is not a terminal' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &CodexCritic{Bin: stub}
	_, err := c.Round(context.Background(), CriticInput{
		Aspect: critic.Lookup("security"), CriticIndex: 1, Round: 1,
		SystemPrompt: "SYS", TaskContext: "T", DiffPatch: "D",
		Cwd: t.TempDir(),
	})
	if !errors.Is(err, ErrTTYRequired) {
		t.Errorf("got %v, want ErrTTYRequired", err)
	}
}

func TestCodexCritic_HappyPath(t *testing.T) {
	dir := t.TempDir()
	stub := dir + "/codex"
	body := `#!/usr/bin/env bash
cat <<'EOF'
{"type":"thread.started","thread_id":"t1"}
{"type":"item.completed","item":{"type":"agent_message","content":"# Critic 1 - round 1 attacks\n\naspect: security\n"}}
{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":5}}
EOF
`
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &CodexCritic{Bin: stub}
	res, err := c.Round(context.Background(), CriticInput{
		Aspect: critic.Lookup("security"), CriticIndex: 1, Round: 1,
		SystemPrompt: "SYS", TaskContext: "T", DiffPatch: "D",
		Cwd: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Round: %v", err)
	}
	if res.ThreadID != "t1" {
		t.Errorf("ThreadID: got %q", res.ThreadID)
	}
	if !strings.Contains(res.Markdown, "Critic 1") {
		t.Errorf("Markdown: %q", res.Markdown)
	}
}
