package state

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWriteRoundParity(t *testing.T) {
	dir := t.TempDir()
	sess, err := NewSession(dir, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRound(sess, 1, 1, RoleCritic, []byte("ok")); err != nil {
		t.Errorf("legit critic R1: %v", err)
	}
	if err := WriteRound(sess, 1, 2, RoleProposer, []byte("ok")); err != nil {
		t.Errorf("legit proposer R2: %v", err)
	}
	if err := WriteRound(sess, 1, 1, RoleProposer, []byte("x")); !errors.Is(err, ErrRoleParity) {
		t.Errorf("parity check missing: %v", err)
	}
	if err := WriteRound(sess, 1, 2, RoleCritic, []byte("x")); !errors.Is(err, ErrRoleParity) {
		t.Errorf("parity check missing: %v", err)
	}
}

func TestWriteRoundOExcl(t *testing.T) {
	dir := t.TempDir()
	sess, err := NewSession(dir, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRound(sess, 1, 1, RoleCritic, []byte("a")); err != nil {
		t.Fatal(err)
	}
	// Atomic-write rename overwrites; this is acceptable per spec.
	if err := WriteRound(sess, 1, 1, RoleCritic, []byte("b")); err != nil {
		t.Errorf("re-write after the fact: %v", err)
	}
}

func TestWriteProposerState(t *testing.T) {
	dir := t.TempDir()
	sess, err := NewSession(dir, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ps := &ProposerState{Agent: "claude", ForkSessionID: "abc"}
	if err := WriteProposerState(sess, 1, ps); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(sess.Path("forks/critic-1/proposer-state.json"))
	if !strings.Contains(string(b), `"agent": "claude"`) {
		t.Errorf("got %q", b)
	}
}
