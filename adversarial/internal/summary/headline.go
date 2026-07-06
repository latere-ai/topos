// Package summary scores contention and renders summary.md.
package summary

import (
	"sort"

	"latere.ai/x/topos/adversarial/internal/ledger"
)

// Score is the contention score: rounds_survived + 1 if re_attacked.
// Pure function.
func Score(r ledger.Record) int {
	rs := r.RoundsSurvived
	if r.RoundIntroduced != nil && r.RoundLastTouched > *r.RoundIntroduced {
		survived := r.RoundLastTouched - *r.RoundIntroduced
		if survived > rs {
			rs = survived
		}
	}
	if r.ReAttacked {
		rs++
	}
	return rs
}

// SortByContention returns records sorted by Score DESC, then
// RoundIntroduced ASC, then AttackID ASC. Total order; deterministic.
func SortByContention(records []ledger.Record) []ledger.Record {
	out := append([]ledger.Record(nil), records...)
	sort.Slice(out, func(i, j int) bool {
		si, sj := Score(out[i]), Score(out[j])
		if si != sj {
			return si > sj
		}
		ri, rj := 0, 0
		if out[i].RoundIntroduced != nil {
			ri = *out[i].RoundIntroduced
		}
		if out[j].RoundIntroduced != nil {
			rj = *out[j].RoundIntroduced
		}
		if ri != rj {
			return ri < rj
		}
		return out[i].AttackID < out[j].AttackID
	})
	return out
}

// PickHeadline returns the unresolved record with the highest score,
// or nil if there are no unresolved records.
func PickHeadline(records []ledger.Record) *ledger.Record {
	unresolved := make([]ledger.Record, 0, len(records))
	for _, r := range records {
		if r.Status == ledger.StatusUnresolved {
			unresolved = append(unresolved, r)
		}
	}
	if len(unresolved) == 0 {
		return nil
	}
	sorted := SortByContention(unresolved)
	r := sorted[0]
	return &r
}
