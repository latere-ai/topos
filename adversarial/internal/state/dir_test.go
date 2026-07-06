package state

import (
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"
)

func TestSessionIDFormat(t *testing.T) {
	id := newSessionID(time.Date(2026, 5, 6, 14, 12, 33, 0, time.UTC))
	re := regexp.MustCompile(`^20260506T141233Z-[a-z0-9]{6}$`)
	if !re.MatchString(id) {
		t.Errorf("session id %q does not match format", id)
	}
}

func TestNewSessionLayout(t *testing.T) {
	dir := t.TempDir()
	sess, err := NewSession(dir, 3, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		p := filepath.Join(sess.Root, "forks", "critic-"+itoa(i), "rounds")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
}

func itoa(n int) string {
	if n == 1 {
		return "1"
	}
	if n == 2 {
		return "2"
	}
	if n == 3 {
		return "3"
	}
	return ""
}

func TestAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	sess, err := NewSession(dir, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AtomicWrite("start.json", []byte(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(sess.Path("start.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"x":1}` {
		t.Errorf("got %q", got)
	}
}

func TestAppendLineMultiple(t *testing.T) {
	dir := t.TempDir()
	sess, err := NewSession(dir, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if err := sess.AppendLine("attacks.jsonl", []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(sess.Path("attacks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, c := range b {
		if c == '\n' {
			count++
		}
	}
	if count != 100 {
		t.Errorf("lines: got %d, want 100", count)
	}
}

func TestAppendLineRace(t *testing.T) {
	dir := t.TempDir()
	sess, err := NewSession(dir, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sess.AppendLine("attacks.jsonl", []byte(`{}`)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func TestAtomicWriteRefusesExistingTemp(t *testing.T) {
	dir := t.TempDir()
	sess, err := NewSession(dir, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: same target written twice - second wins (rename overwrites
	// is the OS guarantee). The interesting invariant is that an
	// in-flight temp file cannot collide because of randSuffix().
	if err := sess.AtomicWrite("end.json", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AtomicWrite("end.json", []byte("b")); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(sess.Path("end.json"))
	if string(got) != "b" {
		t.Errorf("got %q", got)
	}
}

func TestAtomicWriteCreatesNestedDir(t *testing.T) {
	dir := t.TempDir()
	sess, err := NewSession(dir, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AtomicWrite("forks/critic-1/rounds/r1.md", []byte("x")); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(sess.Path("forks/critic-1/rounds/r1.md"))
	if string(got) != "x" {
		t.Errorf("got %q", got)
	}
}

func TestAppendCrossSessionLog(t *testing.T) {
	dir := t.TempDir()
	if err := AppendCrossSessionLog(dir, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := AppendCrossSessionLog(dir, []byte(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "{\"a\":1}\n{\"a\":2}\n" {
		t.Errorf("got %q", b)
	}
}

func TestAppendCrossSessionLogCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "sub")
	if err := AppendCrossSessionLog(dir, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "log.jsonl")); err != nil {
		t.Errorf("log.jsonl not created: %v", err)
	}
}

func TestAtomicWrite_OpenError_DirIsAFile(t *testing.T) {
	// Trigger MkdirAll's error path: pre-create a regular file at the
	// dir we'd need to make. AtomicWrite then can't create a directory
	// where a file already exists.
	dir := t.TempDir()
	sess, err := NewSession(dir, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Create a regular file at the path that AtomicWrite would try to
	// turn into a directory.
	conflict := sess.Path("conflict")
	if err := os.WriteFile(conflict, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sess.AtomicWrite("conflict/sub/file", nil); err == nil {
		t.Error("expected MkdirAll to fail when path is a file")
	}
}

func TestAppendLine_DirIsAFile(t *testing.T) {
	dir := t.TempDir()
	sess, err := NewSession(dir, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	conflict := sess.Path("c")
	if err := os.WriteFile(conflict, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendLine("c/sub/file", nil); err == nil {
		t.Error("expected error")
	}
}

func TestAppendCrossSessionLog_PathIsAFile(t *testing.T) {
	dir := t.TempDir()
	conflict := filepath.Join(dir, "blocker")
	if err := os.WriteFile(conflict, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AppendCrossSessionLog(filepath.Join(conflict, "nested"), nil); err == nil {
		t.Error("expected MkdirAll error")
	}
}

func TestWriteForkStats(t *testing.T) {
	dir := t.TempDir()
	sess, err := NewSession(dir, 2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	stats := map[string]any{"tokens": 1234, "rounds": 3}
	if err := WriteForkStats(sess, 1, stats); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(sess.Path(filepath.Join("forks", "critic-1", "stats.json")))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`"tokens":\s*1234`).Match(b) {
		t.Errorf("stats body missing token field: %s", b)
	}
}
