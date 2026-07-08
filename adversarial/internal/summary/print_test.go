package summary

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/topos/adversarial/internal/ansi"
)

// TestPrintRenderedPlainPassThrough: when styled=false the bytes are
// emitted unchanged. This is the contract piped output relies on:
// `adversarial ... > out.md` must produce a valid markdown file, not one
// peppered with escape codes.
func TestPrintRenderedPlainPassThrough(t *testing.T) {
	body := []byte("# Title\n\nbody\n")
	var buf bytes.Buffer
	if _, err := PrintRendered(&buf, body, false); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), body) {
		t.Errorf("plain mode altered bytes: got %q, want %q", buf.Bytes(), body)
	}
	if strings.Contains(buf.String(), "\x1b[") {
		t.Error("plain mode must not introduce ANSI escapes")
	}
}

// TestPrintRenderedStyledHeadersAndFences: with styled=true headers
// are wrapped in bold+cyan and fenced code blocks are dimmed.
func TestPrintRenderedStyledHeadersAndFences(t *testing.T) {
	body := []byte("# Top\n\n## Section\n\n```\ncode\n```\n\nplain text\n")
	var buf bytes.Buffer
	if _, err := PrintRendered(&buf, body, true); err != nil {
		t.Fatal(err)
	}
	got := buf.String()

	for _, want := range []string{
		ansi.Bold + ansi.Cyan + "# Top" + ansi.Reset,
		ansi.Bold + ansi.Cyan + "## Section" + ansi.Reset,
		ansi.Dim + "```" + ansi.Reset,
		ansi.Dim + "code" + ansi.Reset,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("styled output missing %q. full output:\n%q", want, got)
		}
	}
	// Plain text outside headers/fences must NOT carry escapes.
	if !slices.Contains(strings.Split(got, "\n"), "plain text") {
		t.Errorf("plain text line should pass through unstyled. full:\n%q", got)
	}
}

// TestIsTerminalRegularFile pins the gating: a plain file is never a
// TTY, so PrintRendered called with the result will stay plain. This
// is what protects piped runs from polluting their output with
// escape codes.
func TestIsTerminalRegularFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if IsTerminal(f) {
		t.Error("regular file must not be reported as TTY")
	}
}
