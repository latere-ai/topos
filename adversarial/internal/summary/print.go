package summary

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"strings"

	"latere.ai/x/topos/adversarial/internal/ansi"
)

// IsTerminal reports whether f is attached to an interactive TTY.
// Used to gate ANSI styling on the rendered summary printout: piped
// or redirected output stays plain so `adversarial ... | tee` still
// produces a usable file.
func IsTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

// PrintRendered writes the summary body to w. When styled is true the
// markdown is decorated with ANSI escapes (headers bold-cyan, fenced
// code blocks dimmed); otherwise the bytes are written through
// unchanged. The function never returns an error from styling itself,
// only from the underlying Write.
func PrintRendered(w io.Writer, body []byte, styled bool) (int, error) {
	if !styled {
		return w.Write(body)
	}
	return w.Write(renderANSI(body))
}

func renderANSI(body []byte) []byte {
	var out bytes.Buffer
	inFence := false
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	first := true
	for scanner.Scan() {
		if !first {
			out.WriteByte('\n')
		}
		first = false
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "```"):
			inFence = !inFence
			writeWrapped(&out, line, ansi.Dim)
		case inFence:
			writeWrapped(&out, line, ansi.Dim)
		case strings.HasPrefix(line, "#"):
			writeWrapped(&out, line, ansi.Bold+ansi.Cyan)
		default:
			out.WriteString(line)
		}
	}
	if bytes.HasSuffix(body, []byte("\n")) {
		out.WriteByte('\n')
	}
	return out.Bytes()
}

func writeWrapped(b *bytes.Buffer, line, prefix string) {
	b.WriteString(prefix)
	b.WriteString(line)
	b.WriteString(ansi.Reset)
}
