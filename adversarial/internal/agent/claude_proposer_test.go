package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeProposer_OutputArgs(t *testing.T) {
	p := &ClaudeProposer{}
	if got := p.outputArgs(); strings.Join(got, " ") != "--output-format json" {
		t.Errorf("non-verbose: got %v", got)
	}
	p.Verbose = true
	if got := p.outputArgs(); strings.Join(got, " ") != "--output-format stream-json --verbose" {
		t.Errorf("verbose: got %v", got)
	}
}

func TestClaudeProposer_ToolArgs(t *testing.T) {
	// Default: no tool restriction, no args.
	if got := (&ClaudeProposer{}).toolArgs(); got != nil {
		t.Errorf("default toolArgs = %v, want nil", got)
	}
	// With a denylist: a single comma-joined --disallowedTools arg.
	p := &ClaudeProposer{DisallowedTools: []string{"Write", "Edit", "Bash"}}
	if got := strings.Join(p.toolArgs(), " "); got != "--disallowedTools Write,Edit,Bash" {
		t.Errorf("toolArgs = %q", got)
	}
}

func TestClaudeProposer_NextRound_HappyPath(t *testing.T) {
	if _, err := os.Stat("/usr/bin/env"); err != nil {
		t.Skip("posix env required")
	}

	binDir := t.TempDir()
	stub := filepath.Join(binDir, "claude")
	body := `#!/usr/bin/env bash
cat <<'EOF'
{"type":"result","subtype":"success","session_id":"fork-1","result":"hi","is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}
EOF
`
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	p := &ClaudeProposer{Bin: stub, Cwd: t.TempDir()}
	res, err := p.NextRound(context.Background(), "fork-1", "@some-pointer")
	if err != nil {
		t.Fatalf("NextRound: %v", err)
	}
	if res.Response != "hi" {
		t.Errorf("Response: got %q", res.Response)
	}
	if res.ForkID != "fork-1" {
		t.Errorf("ForkID: got %q", res.ForkID)
	}
}

func TestClaudeProposer_NextRound_UnexpectedFork(t *testing.T) {
	binDir := t.TempDir()
	stub := filepath.Join(binDir, "claude")
	body := `#!/usr/bin/env bash
cat <<'EOF'
{"type":"result","subtype":"success","session_id":"OTHER","result":"hi","is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}
EOF
`
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &ClaudeProposer{Bin: stub, Cwd: t.TempDir()}
	_, err := p.NextRound(context.Background(), "fork-1", "@p")
	if !errors.Is(err, ErrUnexpectedFork) {
		t.Errorf("got %v, want ErrUnexpectedFork", err)
	}
}

func TestClaudeProposer_NextRound_AgentError(t *testing.T) {
	binDir := t.TempDir()
	stub := filepath.Join(binDir, "claude")
	body := `#!/usr/bin/env bash
cat <<'EOF'
{"type":"result","subtype":"error","session_id":"fork-1","result":"boom","is_error":true,"usage":{"input_tokens":1,"output_tokens":1}}
EOF
`
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &ClaudeProposer{Bin: stub, Cwd: t.TempDir()}
	_, err := p.NextRound(context.Background(), "fork-1", "@p")
	if !errors.Is(err, ErrAgentError) {
		t.Errorf("got %v, want ErrAgentError", err)
	}
}

func TestClaudeProposer_NextRound_EmptyResult(t *testing.T) {
	binDir := t.TempDir()
	stub := filepath.Join(binDir, "claude")
	body := `#!/usr/bin/env bash
cat <<'EOF'
{"type":"result","subtype":"success","session_id":"fork-1","result":"","is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}
EOF
`
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &ClaudeProposer{Bin: stub, Cwd: t.TempDir()}
	_, err := p.NextRound(context.Background(), "fork-1", "@p")
	if !errors.Is(err, ErrEmptyResult) {
		t.Errorf("got %v, want ErrEmptyResult", err)
	}
}

func TestClaudeProposer_NextRound_AuthError(t *testing.T) {
	binDir := t.TempDir()
	stub := filepath.Join(binDir, "claude")
	body := `#!/usr/bin/env bash
echo "Authentication error" >&2
exit 1
`
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &ClaudeProposer{Bin: stub, Cwd: t.TempDir()}
	_, err := p.NextRound(context.Background(), "fork-1", "@p")
	if !errors.Is(err, ErrAuth) {
		t.Errorf("got %v, want ErrAuth", err)
	}
}

func TestClaudeProposer_NextRound_CwdMismatch(t *testing.T) {
	binDir := t.TempDir()
	stub := filepath.Join(binDir, "claude")
	body := `#!/usr/bin/env bash
echo "No conversation found with session ID 'fork-1'" >&2
exit 1
`
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &ClaudeProposer{Bin: stub, Cwd: t.TempDir()}
	_, err := p.NextRound(context.Background(), "fork-1", "@p")
	if !errors.Is(err, ErrCwdMismatch) {
		t.Errorf("got %v, want ErrCwdMismatch", err)
	}
}

func TestClaudeProposer_NextRound_BadJSON(t *testing.T) {
	binDir := t.TempDir()
	stub := filepath.Join(binDir, "claude")
	body := `#!/usr/bin/env bash
echo "not-json"
`
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &ClaudeProposer{Bin: stub, Cwd: t.TempDir()}
	_, err := p.NextRound(context.Background(), "fork-1", "@p")
	if !errors.Is(err, ErrJSON) {
		t.Errorf("got %v, want ErrJSON", err)
	}
}
