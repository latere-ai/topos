package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCleanEnvScrubsAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "stale")
	env := CleanEnv()
	for _, kv := range env {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			t.Errorf("API key not scrubbed: %q", kv)
		}
	}
	found := false
	for _, kv := range env {
		if kv == "LC_ALL=C" {
			found = true
			break
		}
	}
	if !found {
		t.Error("LC_ALL=C not set")
	}
}

func TestDecodeJSONLineSanitizes(t *testing.T) {
	// raw byte 0x07 inside a string; standard json.Unmarshal rejects.
	in := []byte("{\"x\":\"a\x07b\"}")
	var got map[string]string
	if err := DecodeJSONLine(in, &got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got["x"], "a") {
		t.Errorf("got %q", got["x"])
	}
}

func TestExecSuccess(t *testing.T) {
	res, err := Exec(context.Background(), Run{
		Bin: "echo", Args: []string{"hello"},
		Cwd: t.TempDir(), Env: CleanEnv(), Deadline: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Stdout), "hello") {
		t.Errorf("stdout: %q", res.Stdout)
	}
}

func TestExecCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	res, err := Exec(ctx, Run{
		Bin: "sh", Args: []string{"-c", "sleep 5"},
		Cwd: t.TempDir(), Env: CleanEnv(), Deadline: 10 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error from cancelled exec")
	}
	if !res.Killed {
		t.Error("expected Killed=true")
	}
	if res.Duration > 4*time.Second {
		t.Errorf("teardown latency too high: %v", res.Duration)
	}
}
