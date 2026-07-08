package summary

import (
	"fmt"
	"strings"

	"latere.ai/x/topos/adversarial/internal/ledger"
	"latere.ai/x/topos/adversarial/internal/round"
	"latere.ai/x/topos/adversarial/internal/state"
)

// Render renders summary.md for one finished run.
type Render struct {
	Format string // "markdown" only in v0
}

// Bytes produces the on-disk summary content.
func (r *Render) Bytes(s *round.Summary, agg map[string]ledger.Record) ([]byte, error) {
	if r.Format == "json" {
		panic("json format not implemented in v0")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Adversarial review - terminated: %s\n\n", s.Termination)

	hl := PickHeadline(values(agg))
	if hl != nil {
		b.WriteString("## Headline (most contested unresolved)\n")
		writeHeadline(&b, *hl)
		b.WriteString("\n")
	}

	others := unresolvedExcept(values(agg), hl)
	if len(others) > 0 {
		fmt.Fprintf(&b, "## Other unresolved (%d, sorted by contention)\n", len(others))
		for _, r := range others {
			writeUnresolved(&b, r)
		}
		b.WriteString("\n")
	}

	resolved := resolvedRecords(values(agg))
	if len(resolved) > 0 {
		fmt.Fprintf(&b, "## Resolved (%d)\n", len(resolved))
		for _, r := range resolved {
			writeResolved(&b, r)
		}
		b.WriteString("\n")
	}

	conceded := countByStatus(agg, ledger.StatusConceded)
	total := len(agg)
	tokens := s.TokensUsed
	fmt.Fprintf(&b, "## Stats\n")
	fmt.Fprintf(&b, "critic-found-bug rate: %d/%d attacks led to a fix\n", conceded, total)
	fmt.Fprintf(&b, "adversarial cost: %d tokens, %d rounds, %d critics\n", tokens, totalRounds(s), len(s.Forks))
	u := s.Usage
	if u.Total() > 0 {
		fmt.Fprintf(&b, "token usage (run): in=%d out=%d cache_create=%d cache_read=%d total=%d cost=$%.4f\n",
			u.Input, u.Output, u.CacheCreate, u.CacheRead, u.Total(), s.USD)
		for _, f := range s.Forks {
			fu := f.Usage.Total
			topic := f.Topic
			if topic == "" {
				topic = "(no topic declared)"
			}
			fmt.Fprintf(&b, "  - fork %d (%s): in=%d out=%d cache_create=%d cache_read=%d total=%d cost=$%.4f\n",
				f.Index, topic, fu.Input, fu.Output, fu.CacheCreate, fu.CacheRead, fu.Total(), f.Usage.TotalUSD)
		}
	}
	if s.Sess != nil {
		fmt.Fprintf(&b, "session: %s\n", s.Sess.Root)
	}
	return []byte(b.String()), nil
}

// SurfacingDecision is the per-run stdout + exit-code triple.
type SurfacingDecision struct {
	Surface    bool
	StdoutLine string
	ExitCode   int
}

// Decide computes the surfacing decision per spec 23.
func Decide(s *round.Summary) SurfacingDecision {
	summaryPath := ""
	if s.Sess != nil {
		summaryPath = s.Sess.Path("summary.md")
	}
	switch s.Termination {
	case round.TermSteadyState:
		if s.Unresolved == 0 {
			return SurfacingDecision{
				StdoutLine: "[adversarial] clean run; see .adversarial/log.jsonl",
			}
		}
		return SurfacingDecision{
			Surface:    true,
			StdoutLine: fmt.Sprintf("[adversarial] %d unresolved; see %s", s.Unresolved, summaryPath),
			ExitCode:   1,
		}
	case round.TermInterrupted:
		return SurfacingDecision{
			Surface:    true,
			StdoutLine: fmt.Sprintf("[adversarial] interrupted (%d known unresolved); partial review at %s", s.Unresolved, summaryPath),
			ExitCode:   130,
		}
	default: // max-turn, cost-cap, malformed-output
		return SurfacingDecision{
			Surface:    true,
			StdoutLine: fmt.Sprintf("[adversarial] terminated %s (%d unresolved); see %s", s.Termination, s.Unresolved, summaryPath),
			ExitCode:   1,
		}
	}
}

func writeHeadline(b *strings.Builder, r ledger.Record) {
	loc := r.Location
	if loc == "" {
		loc = "?"
	}
	fmt.Fprintf(b, "- [%s/%s] %s\n", r.Aspect, loc, oneLine(r.Claim))
	fmt.Fprintf(b, "  - Critic: %s\n", oneLine(r.ExpectedViolation))
	fmt.Fprintf(b, "  - **Stake**: %s\n", oneLine(r.Reproduction))
	fmt.Fprintf(b, "  - Contention: %d (re-attacked: %v)\n", Score(r), r.ReAttacked)
}

func writeUnresolved(b *strings.Builder, r ledger.Record) {
	fmt.Fprintf(b, "- [%s/%s] %s\n", r.Aspect, r.Location, oneLine(r.Claim))
	fmt.Fprintf(b, "  - Stake: %s\n", oneLine(r.Reproduction))
	fmt.Fprintf(b, "  - Contention: %d\n", Score(r))
}

func writeResolved(b *strings.Builder, r ledger.Record) {
	switch r.Status {
	case ledger.StatusConceded:
		fmt.Fprintf(b, "- [conceded] %s → fixed at %s\n", oneLine(r.Claim), strings.Join(r.ConcessionFiles, ", "))
	case ledger.StatusRebutted:
		fmt.Fprintf(b, "- [rebutted] %s\n", oneLine(r.Claim))
	case ledger.StatusWithdrawn:
		fmt.Fprintf(b, "- [withdrawn] %s\n", oneLine(r.Claim))
	}
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	// Truncate on a rune boundary; slicing bytes can split a multibyte
	// UTF-8 rune (the repo ships zh content) and emit mojibake into the
	// rendered summary.md.
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "..."
	}
	return strings.TrimSpace(s)
}

func unresolvedExcept(records []ledger.Record, exclude *ledger.Record) []ledger.Record {
	out := make([]ledger.Record, 0, len(records))
	for _, r := range records {
		if r.Status != ledger.StatusUnresolved {
			continue
		}
		if exclude != nil && r.AttackID == exclude.AttackID {
			continue
		}
		out = append(out, r)
	}
	return SortByContention(out)
}

func resolvedRecords(records []ledger.Record) []ledger.Record {
	out := make([]ledger.Record, 0, len(records))
	for _, r := range records {
		if r.Status == ledger.StatusConceded || r.Status == ledger.StatusRebutted || r.Status == ledger.StatusWithdrawn {
			out = append(out, r)
		}
	}
	return out
}

func values(m map[string]ledger.Record) []ledger.Record {
	out := make([]ledger.Record, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	return out
}

func countByStatus(m map[string]ledger.Record, st ledger.Status) int {
	n := 0
	for _, r := range m {
		if r.Status == st {
			n++
		}
	}
	return n
}

func totalRounds(s *round.Summary) int {
	n := 0
	for _, f := range s.Forks {
		n += f.Rounds
	}
	return n
}

// Persist writes summary.md and end.json for a finished run.
func Persist(s *round.Summary, agg map[string]ledger.Record, exitCode int) error {
	r := &Render{Format: "markdown"}
	b, err := r.Bytes(s, agg)
	if err != nil {
		return err
	}
	if err := s.Sess.AtomicWrite("summary.md", b); err != nil {
		return err
	}
	end := &state.EndFile{
		SessionID:   s.Sess.ID,
		Termination: state.Termination{Reason: string(s.Termination)},
		Stats: state.Stats{
			TotalAttacks: len(agg),
			ByStatus:     statusCounts(agg),
			TokensUsed:   s.TokensUsed,
			TokenUsage:   &s.Usage,
			WallSeconds:  s.WallSeconds,
		},
		ExitCode:    exitCode,
		SummaryPath: "summary.md",
	}
	if hl := PickHeadline(values(agg)); hl != nil {
		end.Headline = &state.HeadlineRef{AttackID: hl.AttackID, Contention: Score(*hl)}
	}
	return state.WriteEnd(s.Sess, end)
}

func statusCounts(m map[string]ledger.Record) map[string]int {
	out := map[string]int{
		"open": 0, "conceded": 0, "rebutted": 0, "withdrawn": 0, "unresolved": 0,
	}
	for _, r := range m {
		out[string(r.Status)]++
	}
	return out
}
