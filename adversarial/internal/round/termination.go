// Package round is the orchestration loop.
package round

// TerminationReason enumerates the on-disk values for end.json's
// termination.reason field.
type TerminationReason string

// TerminationReason values.
const (
	// TermSteadyState fires when no new attacks two rounds running.
	TermSteadyState TerminationReason = "steady-state"
	// TermMaxTurn fires at the per-fork turn cap.
	TermMaxTurn TerminationReason = "max-turn"
	// TermCostCap fires at the run-level token budget.
	TermCostCap TerminationReason = "cost-cap"
	// TermMalformedOutput fires after two consecutive malformed critic
	// outputs.
	TermMalformedOutput TerminationReason = "malformed-output"
	// TermInterrupted fires on SIGINT/SIGTERM.
	TermInterrupted TerminationReason = "interrupted"
)

// ForkHistory is the per-fork state the detector needs.
type ForkHistory struct {
	Round         int
	NewAttacks    int
	ReAttacks     int
	Withdrawn     int
	ParseErrors   int
	MalformedFlag bool
}

// Detector is a value-typed bundle of detection rules.
type Detector struct {
	MaxRounds int
	CostCap   int
}

// SteadyState requires at least three critic rounds in history; returns
// true iff the last two have zero new attacks and zero re-attacks.
func (d *Detector) SteadyState(history []ForkHistory) bool {
	if len(history) < 3 {
		return false
	}
	last := history[len(history)-1]
	prev := history[len(history)-2]
	return last.NewAttacks == 0 && last.ReAttacks == 0 &&
		prev.NewAttacks == 0 && prev.ReAttacks == 0
}

// MalformedTwice returns true when the last two critic rounds in
// history both have MalformedFlag = true.
func (d *Detector) MalformedTwice(history []ForkHistory) bool {
	if len(history) < 2 {
		return false
	}
	return history[len(history)-1].MalformedFlag && history[len(history)-2].MalformedFlag
}

// CostMeter accumulates token usage across all subprocess calls.
type CostMeter struct {
	cap     int
	used    int
	perCall []int
}

// NewCostMeter returns a meter capped at capTokens.
func NewCostMeter(capTokens int) *CostMeter {
	return &CostMeter{cap: capTokens}
}

// Add records tokens for one call.
func (c *CostMeter) Add(tokens int) {
	c.used += tokens
	c.perCall = append(c.perCall, tokens)
}

// Used returns the total tokens consumed.
func (c *CostMeter) Used() int { return c.used }

// ExceedsCap returns true iff used >= cap.
func (c *CostMeter) ExceedsCap() bool { return c.used >= c.cap }

// EstimateTokens is the 4-chars/token fallback when the agent doesn't
// report usage.
func EstimateTokens(promptAndResponse string) int {
	return (len(promptAndResponse) + 3) / 4
}
