// Package input reads the claude session transcript and computes the
// working-tree diff. See specs/05-inputs.md for design.
package input

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Transcript captures the parsed metadata of a root claude session.
type Transcript struct {
	Path      string
	SessionID string
	FirstUser string
	StartedAt time.Time
	LineCount int
}

var (
	// ErrTranscriptNotFound wraps os.ErrNotExist with the searched path.
	ErrTranscriptNotFound = errors.New("transcript not found")
	// ErrTranscriptMalformed signals > 5% bad lines or a parse error
	// past tolerance.
	ErrTranscriptMalformed = errors.New("transcript malformed")
	// ErrNoUserTurn signals the transcript contains zero user records.
	ErrNoUserTurn = errors.New("transcript has no user turn")
)

// EncodeCwd encodes an absolute cwd into the segment claude uses under
// ~/.claude/projects/<encoded>/. Claude replaces both `/` and `.` with
// `-`, so /Users/x/dev/changkun.de/agents-byzantine-tolerance becomes
// -Users-x-dev-changkun-de-agents-byzantine-tolerance. The encoding is
// many-to-one (a directory containing `-` or `.` cannot be
// distinguished from a path boundary), so DecodeCwd is best-effort.
//
//	/Users/changkun/dev/foo  ->  -Users-changkun-dev-foo
func EncodeCwd(cwd string) string {
	s := filepath.ToSlash(cwd)
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, ".", "-")
	return s
}

// DecodeCwd is a best-effort inverse of EncodeCwd. It replaces every
// `-` with `/`, which loses information when the original cwd contained
// `.` or `-` characters. Use the returned string only as a hint; for a
// reliable equality check, compare encoded forms via EncodeCwd.
func DecodeCwd(encoded string) string {
	return strings.ReplaceAll(encoded, "-", "/")
}

// LocateTranscript resolves the on-disk path for a root session.
//
// Preference order:
//  1. explicit (when non-empty): used as-is.
//  2. ~/.claude/projects/<encoded-cwd>/<sessionID>.jsonl
//
// Returns ErrTranscriptNotFound when neither path exists.
func LocateTranscript(home, cwd, sessionID, explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit, nil
		}
		return "", fmt.Errorf("%w: %s", ErrTranscriptNotFound, explicit)
	}
	if home == "" || cwd == "" || sessionID == "" {
		return "", fmt.Errorf("%w: missing home/cwd/sessionID", ErrTranscriptNotFound)
	}
	p := filepath.Join(home, ".claude", "projects", EncodeCwd(cwd), sessionID+".jsonl")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("%w: %s", ErrTranscriptNotFound, p)
}

// FindSession scans ~/.claude/projects/*/<sessionID>.jsonl and returns
// the on-disk path plus the encoded segment it lives under. The encoded
// segment is the directory name claude assigned, which is reliable;
// the decoded cwd is intrinsically lossy (see DecodeCwd) and is left to
// the caller to compute as a hint.
//
// Returns ErrTranscriptNotFound if no project directory contains the
// session.
func FindSession(home, sessionID string) (path, encodedSegment string, err error) {
	if home == "" || sessionID == "" {
		return "", "", fmt.Errorf("%w: missing home/sessionID", ErrTranscriptNotFound)
	}
	projects := filepath.Join(home, ".claude", "projects")
	entries, err := os.ReadDir(projects)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrTranscriptNotFound, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(projects, e.Name(), sessionID+".jsonl")
		if _, err := os.Stat(p); err == nil {
			return p, e.Name(), nil
		}
	}
	return "", "", fmt.Errorf("%w: session %s not found under %s", ErrTranscriptNotFound, sessionID, projects)
}

// ReadTranscript opens a JSONL transcript and returns a populated
// *Transcript. Streaming line-by-line; bounded memory.
func ReadTranscript(path string) (*Transcript, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	t := &Transcript{
		Path:      path,
		SessionID: strings.TrimSuffix(filepath.Base(path), ".jsonl"),
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var bad int
	var first []byte
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(line, &probe); err != nil {
			bad++
			continue
		}
		t.LineCount++
		if t.StartedAt.IsZero() {
			if ts, ok := probe["timestamp"]; ok {
				_ = json.Unmarshal(ts, &t.StartedAt)
			}
		}
		if first == nil {
			if typ, ok := probe["type"]; ok {
				var s string
				if err := json.Unmarshal(typ, &s); err == nil && s == "user" {
					first = append([]byte(nil), line...)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	// Ratio is against every non-empty line, including bad ones, and is
	// not gated on LineCount>0: a transcript where every line is
	// unparseable (LineCount==0, bad>0) is malformed, not a missing user
	// turn. Empty files (total==0) still fall through to ErrNoUserTurn.
	if total := t.LineCount + bad; total > 0 && float64(bad)/float64(total) > 0.05 {
		return nil, ErrTranscriptMalformed
	}
	if first == nil {
		return nil, ErrNoUserTurn
	}
	t.FirstUser, err = ExtractFirstUser([][]byte{first})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// ExtractFirstUser walks records in order and returns the message
// content of the first user record. String-form and array-of-parts form
// are both supported; tool_use parts are skipped.
func ExtractFirstUser(records [][]byte) (string, error) {
	for _, line := range records {
		var rec struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Type != "user" {
			continue
		}
		// String form?
		var s string
		if err := json.Unmarshal(rec.Message.Content, &s); err == nil {
			return s, nil
		}
		// Array form.
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(rec.Message.Content, &parts); err == nil {
			var b strings.Builder
			for _, p := range parts {
				if p.Type != "text" {
					continue
				}
				if b.Len() > 0 {
					b.WriteString("\n\n")
				}
				b.WriteString(p.Text)
			}
			return b.String(), nil
		}
	}
	return "", ErrNoUserTurn
}
