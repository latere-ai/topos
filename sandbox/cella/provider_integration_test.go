//go:build integration

package cella

import (
	"context"
	"os"
	"testing"
	"time"

	"latere.ai/x/agents/internal/sandbox"
)

// TestIntegrationLifecycle exercises the full sandbox lifecycle against a
// real Cella instance. It is gated by the //go:build integration tag so
// that CI without a live Cella endpoint stays green.
//
// Required environment variables:
//
//	CELLA_BASE_URL — base URL of the Cella API (e.g. https://sandbox.latere.ai)
//	CELLA_TOKEN    — bearer token with write:sandbox + exec:sandbox scopes
//
// Run with:
//
//	go test -tags=integration ./internal/sandbox/cella/... \
//	  -run TestIntegrationLifecycle -v -timeout 5m
func TestIntegrationLifecycle(t *testing.T) {
	baseURL := os.Getenv("CELLA_BASE_URL")
	token := os.Getenv("CELLA_TOKEN")
	if baseURL == "" || token == "" {
		t.Skip("CELLA_BASE_URL and CELLA_TOKEN not set; skipping integration test")
	}

	p := NewCellaSandboxProvider(baseURL, NewStaticTokenSource(token))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	t.Log("creating sandbox...")
	sb, err := p.Create(ctx, sandbox.CreateOptions{
		Name:   "topos-integration-test",
		Labels: map[string]string{"test": "true"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Logf("created sandbox id=%s name=%s state=%s", sb.ID, sb.Name, sb.State)

	// Clean up even on test failure.
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanCancel()
		if err := p.Destroy(cleanCtx, sb.ID); err != nil {
			t.Logf("cleanup Destroy(%s): %v (non-fatal)", sb.ID, err)
		}
	})

	// Wait for the sandbox to become running (poll HealthCheck, max 60s).
	t.Log("waiting for sandbox to reach running state...")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if err := p.HealthCheck(ctx, sb.ID); err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err := p.HealthCheck(ctx, sb.ID); err != nil {
		t.Fatalf("HealthCheck after wait: %v", err)
	}

	// Exec a simple command.
	t.Log("running exec...")
	res, err := p.Exec(ctx, sb.ID, sandbox.ExecOptions{
		Argv: []string{"sh", "-c", "echo integration-test-ok; exit 0"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	t.Logf("exec result: phase=%s exitCode=%d stdout=%q", res.Phase, res.ExitCode, res.Stdout)
	if res.ExitCode != 0 {
		t.Errorf("Exec: exit %d, want 0", res.ExitCode)
	}

	// WriteFile + ReadFile round-trip.
	t.Log("testing WriteFile/ReadFile...")
	content := []byte("integration round-trip\x00\xff")
	if err := p.WriteFile(ctx, sb.ID, "/tmp/topos_test_file", content); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := p.ReadFile(ctx, sb.ID, "/tmp/topos_test_file")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("ReadFile: got %q, want %q", got, content)
	}

	// ListFiles.
	t.Log("testing ListFiles...")
	files, err := p.ListFiles(ctx, sb.ID, "/tmp")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	found := false
	for _, f := range files {
		if f.Name == "topos_test_file" {
			found = true
		}
	}
	if !found {
		t.Errorf("ListFiles: topos_test_file not found in /tmp; entries: %v", files)
	}

	t.Log("integration lifecycle complete")
}
