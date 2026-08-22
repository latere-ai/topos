package summary

import (
	"math/rand"
	"testing"

	"latere.ai/x/topos/adversarial/internal/ledger"
)

func recs() []ledger.Record {
	return []ledger.Record{
		{AttackID: "c1-1", RoundIntroduced: new(1), RoundLastTouched: 5, ReAttacked: true, Status: ledger.StatusUnresolved},  // 4+1=5
		{AttackID: "c1-3", RoundIntroduced: new(1), RoundLastTouched: 5, ReAttacked: false, Status: ledger.StatusUnresolved}, // 4
		{AttackID: "c2-2", RoundIntroduced: new(3), RoundLastTouched: 5, ReAttacked: true, Status: ledger.StatusUnresolved},  // 2+1=3
		{AttackID: "c1-7", RoundIntroduced: new(1), RoundLastTouched: 1, ReAttacked: false, Status: ledger.StatusConceded},   // skipped
	}
}

func TestPickHeadline(t *testing.T) {
	got := PickHeadline(recs())
	if got == nil || got.AttackID != "c1-1" {
		t.Errorf("got %+v", got)
	}
}

func TestPickHeadlineEmpty(t *testing.T) {
	if r := PickHeadline([]ledger.Record{}); r != nil {
		t.Errorf("expected nil, got %+v", r)
	}
}

func TestDeterministicAcrossShuffles(t *testing.T) {
	base := recs()
	first := PickHeadline(base)
	for range 100 {
		shuffled := append([]ledger.Record(nil), base...)
		rand.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		got := PickHeadline(shuffled)
		if got.AttackID != first.AttackID {
			t.Errorf("non-deterministic: %s vs %s", first.AttackID, got.AttackID)
		}
	}
}
