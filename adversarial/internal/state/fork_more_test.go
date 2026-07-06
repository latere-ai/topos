package state

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParsePorcelain(t *testing.T) {
	in := " M file_one.go\n?? new_file.go\nA  added.go\n"
	got := parsePorcelain(in)
	want := []string{"file_one.go", "new_file.go", "added.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestParsePorcelainRenameAndQuoted pins the destination-path handling
// for rename/copy lines and the unquoting of paths git wraps in double
// quotes. Before the fix a rename returned the bogus "orig -> dest"
// token and a quoted path kept its surrounding quotes, neither matching
// a real on-disk path.
func TestParsePorcelainRenameAndQuoted(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"rename", "R  old.go -> new.go\n", []string{"new.go"}},
		{"copy", "C  src.go -> dst.go\n", []string{"dst.go"}},
		{"quoted", "A  \"sp ace.txt\"\n", []string{"sp ace.txt"}},
		{"plain", " M file.go\n", []string{"file.go"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parsePorcelain(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestChangedFilesAfter(t *testing.T) {
	dir := t.TempDir()
	for _, c := range [][]string{
		{"git", "init", "-q"},
		{"git", "-c", "user.email=t@e.com", "-c", "user.name=t", "commit", "--allow-empty", "-q", "-m", "init"},
	} {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Dir = dir
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", c, err, b)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "added.go"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ChangedFilesAfter(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range got {
		if strings.HasSuffix(p, "added.go") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected added.go in %v", got)
	}
}

func TestWriteEndAndForkDiff(t *testing.T) {
	sess, err := NewSession(t.TempDir(), 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteEnd(sess, &EndFile{
		SessionID: sess.ID, Termination: Termination{Reason: "steady-state"},
		Stats: Stats{TotalAttacks: 1}, ExitCode: 0,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sess.Path("end.json")); err != nil {
		t.Fatal(err)
	}
	if err := WriteForkDiff(sess, 1, "diff body"); err != nil {
		t.Fatal(err)
	}
	if err := WriteRunDiff(sess, "run diff body"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sess.Path("forks/critic-1/diff.patch")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sess.Path("diff.patch")); err != nil {
		t.Fatal(err)
	}
}

func TestNewSessionRejectsZeroForks(t *testing.T) {
	_, err := NewSession(t.TempDir(), 0, time.Now())
	if err == nil {
		t.Error("expected error for forkCount=0")
	}
}
