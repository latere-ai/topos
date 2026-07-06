package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/adversarial/internal/critic"
)

func buildHelper(t *testing.T, name, src string) string {
	t.Helper()
	dir := t.TempDir()
	gosrc := filepath.Join(dir, "main.go")
	if err := os.WriteFile(gosrc, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, name)
	c := exec.Command("go", "build", "-o", out, gosrc)
	if b, err := c.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, b)
	}
	return out
}

func TestCodexCriticEmptyContent(t *testing.T) {
	bin := buildHelper(t, "empty-codex", `package main
import "fmt"
func main() {
	fmt.Println(`+"`"+`{"type":"thread.started","thread_id":"x"}`+"`"+`)
	fmt.Println(`+"`"+`{"type":"thread.completed"}`+"`"+`)
}
`)
	c := &CodexCritic{Bin: bin}
	_, err := c.Round(context.Background(), CriticInput{
		Aspect: critic.Lookup("security"), CriticIndex: 1, Round: 1,
		SystemPrompt: "x", TaskContext: "t", DiffPatch: "d",
		Cwd: t.TempDir(), Deadline: 5 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected ErrEmptyContent, got %v", err)
	}
}

func TestCodexCriticStderrMappings(t *testing.T) {
	bin := buildHelper(t, "rl-codex", `package main
import (
	"fmt"
	"os"
)
func main() {
	fmt.Fprintln(os.Stderr, "error: rate limit exceeded")
	os.Exit(1)
}
`)
	c := &CodexCritic{Bin: bin}
	_, err := c.Round(context.Background(), CriticInput{
		Aspect: critic.Lookup("security"), CriticIndex: 1, Round: 1,
		Cwd: t.TempDir(), Deadline: 5 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("got %v", err)
	}
}

// TestCodexCriticUsageFromTurnCompleted: codex --json terminates the
// stream with a turn.completed event whose usage block carries
// input_tokens (incl. cached), cached_input_tokens, output_tokens,
// reasoning_output_tokens. The parser used to ignore this and report
// zeros, so per-fork usage / cost-cap accounting under-counted by
// 100% on codex critics. Confirmed against codex-cli 0.128.0
// observed shape.
func TestCodexCriticUsageFromTurnCompleted(t *testing.T) {
	bin := buildHelper(t, "usage-codex", `package main
import "fmt"
func main() {
	fmt.Println(`+"`"+`{"type":"thread.started","thread_id":"t"}`+"`"+`)
	fmt.Println(`+"`"+`{"type":"turn.started"}`+"`"+`)
	fmt.Println(`+"`"+`{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"# Critic 1 - round 1 attacks\n\naspect: security\n"}}`+"`"+`)
	fmt.Println(`+"`"+`{"type":"turn.completed","usage":{"input_tokens":15513,"cached_input_tokens":7552,"output_tokens":26,"reasoning_output_tokens":19}}`+"`"+`)
}
`)
	c := &CodexCritic{Bin: bin}
	res, err := c.Round(context.Background(), CriticInput{
		Aspect: critic.Lookup("security"), CriticIndex: 1, Round: 1,
		SystemPrompt: "x", TaskContext: "t", DiffPatch: "d",
		Cwd: t.TempDir(), Deadline: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	// input_tokens=15513 includes the cached portion; the fresh-input
	// bucket is the difference.
	wantInput := 15513 - 7552
	if res.Usage.Input != wantInput {
		t.Errorf("Usage.Input: got %d, want %d (input_tokens - cached_input_tokens)", res.Usage.Input, wantInput)
	}
	if res.Usage.CacheRead != 7552 {
		t.Errorf("Usage.CacheRead: got %d, want 7552 (cached_input_tokens)", res.Usage.CacheRead)
	}
	wantOutput := 26 + 19
	if res.Usage.Output != wantOutput {
		t.Errorf("Usage.Output: got %d, want %d (output + reasoning_output)", res.Usage.Output, wantOutput)
	}
	if res.Usage.CacheCreate != 0 {
		t.Errorf("Usage.CacheCreate: got %d, want 0 (codex does not surface cache_creation)", res.Usage.CacheCreate)
	}
	if res.Tokens != wantInput+wantOutput {
		t.Errorf("Tokens convenience: got %d, want %d", res.Tokens, wantInput+wantOutput)
	}
}

// TestCodexCriticUsageFallbackAnthropicShape: future codex revisions
// might emit anthropic-style cache_read_input_tokens /
// cache_creation_input_tokens directly. Tolerate that shape so the
// parser keeps producing meaningful numbers across CLI updates.
func TestCodexCriticUsageFallbackAnthropicShape(t *testing.T) {
	bin := buildHelper(t, "usage-codex-anthropic", `package main
import "fmt"
func main() {
	fmt.Println(`+"`"+`{"type":"thread.started","thread_id":"t"}`+"`"+`)
	fmt.Println(`+"`"+`{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"# Critic 1 - round 1 attacks\n\naspect: security\n"}}`+"`"+`)
	fmt.Println(`+"`"+`{"type":"turn.completed","usage":{"input_tokens":1000,"output_tokens":200,"cache_read_input_tokens":500,"cache_creation_input_tokens":300}}`+"`"+`)
}
`)
	c := &CodexCritic{Bin: bin}
	res, err := c.Round(context.Background(), CriticInput{
		Aspect: critic.Lookup("security"), CriticIndex: 1, Round: 1,
		SystemPrompt: "x", TaskContext: "t", DiffPatch: "d",
		Cwd: t.TempDir(), Deadline: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage.Input != 1000 {
		t.Errorf("Input: got %d, want 1000", res.Usage.Input)
	}
	if res.Usage.Output != 200 {
		t.Errorf("Output: got %d, want 200", res.Usage.Output)
	}
	if res.Usage.CacheRead != 500 {
		t.Errorf("CacheRead: got %d, want 500", res.Usage.CacheRead)
	}
	if res.Usage.CacheCreate != 300 {
		t.Errorf("CacheCreate: got %d, want 300", res.Usage.CacheCreate)
	}
}

// TestClaudeCriticVerboseSurfacesEvents drives the verbose stream
// path: the mock emits stream-json events including tool_use, then
// the final result. The critic must (a) tee tool/thinking lines to
// EventOut while running, (b) extract Markdown/Usage/USD from the
// final result event. Together these prove verbose streaming shows
// what the agent is doing instead of just "still running".
func TestClaudeCriticVerboseSurfacesEvents(t *testing.T) {
	bin := buildHelper(t, "verbose-claude", `package main
import "fmt"
func main() {
	fmt.Println(`+"`"+`{"type":"system","subtype":"init","session_id":"s"}`+"`"+`)
	fmt.Println(`+"`"+`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"Look at the diff first"}]}}`+"`"+`)
	fmt.Println(`+"`"+`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u1","name":"Read","input":{"file_path":"/repo/x.go"}}]}}`+"`"+`)
	fmt.Println(`+"`"+`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"u1","content":"ok"}]}}`+"`"+`)
	fmt.Println(`+"`"+`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u2","name":"Grep","input":{"pattern":"TODO"}}]}}`+"`"+`)
	fmt.Println(`+"`"+`{"type":"result","subtype":"success","session_id":"s","result":"# Critic 1 - round 1 attacks\n\naspect: security\n","is_error":false,"total_cost_usd":0.0125,"usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":10,"cache_read_input_tokens":20}}`+"`"+`)
}
`)
	var buf strings.Builder
	c := &ClaudeCritic{Bin: bin, Verbose: true, EventOut: &buf}
	res, err := c.Round(context.Background(), CriticInput{
		Aspect: critic.Lookup("security"), CriticIndex: 1, Round: 1,
		SystemPrompt: "x", TaskContext: "t", DiffPatch: "d",
		Cwd: t.TempDir(), Deadline: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"  thinking: Look at the diff first",
		"  → Read: /repo/x.go",
		"  → Grep: TODO",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("EventOut missing %q. full output:\n%s", want, out)
		}
	}
	if !strings.Contains(res.Markdown, "Critic 1 - round 1 attacks") {
		t.Errorf("final Markdown not extracted from result event: %q", res.Markdown)
	}
	if res.Usage.Input != 100 || res.Usage.Output != 50 {
		t.Errorf("Usage from result event: %+v", res.Usage)
	}
	if res.USD != 0.0125 {
		t.Errorf("USD from result event: %v", res.USD)
	}
}

// TestClaudeCriticConciseStaysSingleShot pins the contract that
// non-verbose mode keeps using --output-format json (single-shot)
// and does not feed events to EventOut. This rules out a
// regression where a stale Verbose=true setting from a previous
// caller leaks into a concise run.
func TestClaudeCriticConciseStaysSingleShot(t *testing.T) {
	bin := buildHelper(t, "concise-claude", `package main
import "fmt"
func main() {
	fmt.Println(`+"`"+`{"type":"result","subtype":"success","session_id":"s","result":"# Critic 1 - round 1 attacks\n\naspect: security\n","is_error":false,"usage":{"input_tokens":1,"output_tokens":2}}`+"`"+`)
}
`)
	var buf strings.Builder
	c := &ClaudeCritic{Bin: bin, Verbose: false, EventOut: &buf}
	if _, err := c.Round(context.Background(), CriticInput{
		Aspect: critic.Lookup("security"), CriticIndex: 1, Round: 1,
		SystemPrompt: "x", TaskContext: "t", DiffPatch: "d",
		Cwd: t.TempDir(), Deadline: 5 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("concise mode should not write to EventOut; got: %q", buf.String())
	}
}

// TestCodexCriticVerboseSurfacesToolCalls covers the codex side of
// the same UX: tool_call / command_execution events emitted during
// the run are surfaced live; the final usage from turn.completed
// still reaches CriticResult.Usage.
func TestCodexCriticVerboseSurfacesToolCalls(t *testing.T) {
	bin := buildHelper(t, "verbose-codex", `package main
import "fmt"
func main() {
	fmt.Println(`+"`"+`{"type":"thread.started","thread_id":"t"}`+"`"+`)
	fmt.Println(`+"`"+`{"type":"item.completed","item":{"type":"function_call","name":"read_file","input":{"path":"results/README.md"}}}`+"`"+`)
	fmt.Println(`+"`"+`{"type":"item.completed","item":{"type":"command_execution","command":"git diff HEAD~1"}}`+"`"+`)
	fmt.Println(`+"`"+`{"type":"item.completed","item":{"type":"agent_message","text":"# Critic 1 - round 1 attacks\n\naspect: security\n"}}`+"`"+`)
	fmt.Println(`+"`"+`{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":500,"output_tokens":50}}`+"`"+`)
}
`)
	var buf strings.Builder
	c := &CodexCritic{Bin: bin, Verbose: true, EventOut: &buf}
	res, err := c.Round(context.Background(), CriticInput{
		Aspect: critic.Lookup("security"), CriticIndex: 1, Round: 1,
		SystemPrompt: "x", TaskContext: "t", DiffPatch: "d",
		Cwd: t.TempDir(), Deadline: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"  → read_file: results/README.md",
		"  → shell: git diff HEAD~1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("codex verbose EventOut missing %q. full:\n%s", want, out)
		}
	}
	// agent_message now surfaces as a "text:" preview (was dropped
	// before; the user's session had a codex critic that emitted
	// only shell + agent_message and looked silent). Make sure
	// it's there so a regression doesn't return us to the silent
	// state.
	if !strings.Contains(out, "  text: ") {
		t.Errorf("expected agent_message to surface as 'text:' preview; got:\n%s", out)
	}
	if res.Usage.Input != 500 { // 1000 - 500 cached
		t.Errorf("Usage.Input from turn.completed: got %d, want 500", res.Usage.Input)
	}
}

func TestClaudeProposerCwdMismatch(t *testing.T) {
	bin := buildHelper(t, "noconv-claude", `package main
import (
	"fmt"
	"os"
)
func main() {
	fmt.Fprintln(os.Stderr, "Error: No conversation found with session ID abc")
	os.Exit(1)
}
`)
	p := &ClaudeProposer{Bin: bin, Cwd: t.TempDir(), RootID: "abc"}
	_, err := p.FirstRound(context.Background(), "ping")
	if err == nil || !strings.Contains(err.Error(), "cwd mismatch") {
		t.Errorf("expected ErrCwdMismatch, got %v", err)
	}
}

func TestClaudeProposerAuthError(t *testing.T) {
	bin := buildHelper(t, "auth-claude", `package main
import (
	"fmt"
	"os"
)
func main() {
	fmt.Fprintln(os.Stderr, "Authentication error: 401 Unauthorized")
	os.Exit(1)
}
`)
	p := &ClaudeProposer{Bin: bin, Cwd: t.TempDir(), RootID: "abc"}
	_, err := p.FirstRound(context.Background(), "ping")
	if err == nil || !strings.Contains(err.Error(), "auth") {
		t.Errorf("expected ErrAuth, got %v", err)
	}
}
