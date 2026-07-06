package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestExecEmptyBin(t *testing.T) {
	_, err := Exec(context.Background(), Run{Bin: "", Cwd: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "empty Bin") {
		t.Errorf("got %v", err)
	}
}

func TestExecEmptyCwd(t *testing.T) {
	_, err := Exec(context.Background(), Run{Bin: "/bin/echo"})
	if err == nil || !strings.Contains(err.Error(), "empty Cwd") {
		t.Errorf("got %v", err)
	}
}

func TestExecNonZeroExit(t *testing.T) {
	res, err := Exec(context.Background(), Run{
		Bin: "sh", Args: []string{"-c", "exit 7"},
		Cwd: t.TempDir(), Env: CleanEnv(),
	})
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode: got %d, want 7", res.ExitCode)
	}
}

func TestStreamJSONIgnoresGarbage(t *testing.T) {
	in := bytes.NewReader([]byte("{\"a\":1}\nnotjson\n{\"b\":2}\n"))
	var got []string
	if err := StreamJSON(in, func(raw json.RawMessage) error {
		got = append(got, string(raw))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 valid lines, got %d: %v", len(got), got)
	}
}

// TestStreamJSONLargeLine guards the scanner buffer cap: a single JSON
// line between the old 1 MB limit and the 8 MB limit used by the
// streaming Exec path and the buffered claude parser must parse, not
// error. Before the caps were unified StreamJSON returned
// bufio.ErrTooLong here, failing an otherwise-valid codex critic round.
func TestStreamJSONLargeLine(t *testing.T) {
	// ~2 MB of payload, comfortably over the old 1 MB cap and under 8 MB.
	big := strings.Repeat("x", 2*1024*1024)
	line := `{"text":"` + big + `"}` + "\n"
	in := strings.NewReader(line)
	var got int
	if err := StreamJSON(in, func(json.RawMessage) error {
		got++
		return nil
	}); err != nil {
		t.Fatalf("StreamJSON on a 2 MB line: %v", err)
	}
	if got != 1 {
		t.Errorf("visited %d lines, want 1", got)
	}
}

func TestDecodeJSONLineFailureIncludesPreview(t *testing.T) {
	var dst map[string]string
	err := DecodeJSONLine([]byte(`not json at all`), &dst)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "preview") {
		t.Errorf("err missing preview: %v", err)
	}
}
