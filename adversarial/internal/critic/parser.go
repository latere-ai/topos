package critic

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Disposition discriminates introduce / re-attack / withdraw.
type Disposition int

// Disposition values.
const (
	// DispIntroduce is a fresh attack.
	DispIntroduce Disposition = iota
	// DispReAttack continues a prior attack.
	DispReAttack
	// DispWithdraw retires a prior attack.
	DispWithdraw
)

// Attack is one parsed (and normalized) attack from a critic round.
type Attack struct {
	AttackID          string
	CriticIndex       int
	Aspect            string
	Round             int
	RoundIntroduced   int
	Disposition       Disposition
	Location          string
	Claim             string
	ExpectedViolation string
	Reproduction      string
	WithdrawReason    string
}

// ParseStats summarizes filter outcomes.
type ParseStats struct {
	Total              int
	KeptIntroduce      int
	KeptReAttack       int
	KeptWithdraw       int
	DroppedNoReproduce int
	DroppedStyle       int
	DroppedCrossAspect int
	// DroppedMalformedHeader counts sections whose "## " header did not
	// match the expected shape and were skipped. Surfacing it keeps the
	// invariant Total = sum(Kept*) + sum(Dropped*) so an attack lost to a
	// slightly malformed header is visible in operator diagnostics.
	DroppedMalformedHeader int
	Renamed                int
}

// ParseOption tunes parser behavior.
type ParseOption struct {
	AllowStyleAttacks bool
}

var (
	headerRE      = regexp.MustCompile(`^# Critic\s+\d+\s+-\s+round\s+\d+\s+attacks\s*$`)
	aspectLineRE  = regexp.MustCompile(`^aspect:\s*(.+)$`)
	sectionHeadRE = regexp.MustCompile(`^##\s+(\S+)\s+\[(.+?)\](?:\s+\((re-attack|withdraw)\))?\s*$`)
	idRE          = regexp.MustCompile(`^c(\d+)-(\d+)$`)
	stylePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)should be (named|called|written as|shorter|more idiomatic|simpler)`),
		regexp.MustCompile(`(?i)(naming|formatting|style) (preference|convention)`),
		regexp.MustCompile(`(?i)consider (renaming|reformatting|restyling)`),
	}
	concretePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(panic|crash|leak|inject|bypass|corrupt|race|deadlock|OOM|hang|timeout)\b`),
		regexp.MustCompile(`(?i)(incorrect output|wrong result|silently swallowed|breaks contract)`),
	}
	fencePattern = regexp.MustCompile("(?s)```[^\\n]*\\n(.*?)```")
)

// ExtractDeclaredAspect returns the topic the critic wrote on the
// "aspect:" line of its markdown reply, or "" if the line is missing
// or empty. Used by the round loop to capture the topic the critic
// chose in R1 so subsequent rounds can lock to it.
func ExtractDeclaredAspect(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		if m := aspectLineRE.FindStringSubmatch(line); m != nil {
			return strings.TrimSpace(m[1])
		}
	}
	return ""
}

// Parse is the canonical reader. See specs/02-protocol.md.
func Parse(raw string, expectedAspect string, criticIndex, round int, priorAttackIDs []string, opt ParseOption) ([]Attack, ParseStats, error) {
	priorSet := map[string]bool{}
	for _, id := range priorAttackIDs {
		priorSet[id] = true
	}

	lines := strings.Split(raw, "\n")
	stats := ParseStats{}

	// Validate top header. Tolerant of any preamble the agent emits
	// before the document; scan for the first line that matches.
	headerIdx := -1
	for i, line := range lines {
		if headerRE.MatchString(line) {
			headerIdx = i
			break
		}
	}
	if headerIdx < 0 {
		return nil, stats, fmt.Errorf("missing top header (no line matched %q)", headerRE.String())
	}
	lines = lines[headerIdx:]

	// Find aspect line.
	gotAspect := ""
	for _, line := range lines {
		if m := aspectLineRE.FindStringSubmatch(line); m != nil {
			gotAspect = strings.TrimSpace(m[1])
			break
		}
	}
	if gotAspect == "" {
		gotAspect = expectedAspect
	}

	// Tokenize sections.
	type section struct {
		header string
		body   []string
	}
	var sections []section
	var cur *section
	// Track fenced code blocks so a "## " line inside a reproduction
	// fence (critics routinely quote markdown counterexamples) is not
	// mistaken for a new section header, which would split the body and
	// drop the attack as DroppedNoReproduce.
	inFence := false
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
		}
		if !inFence && strings.HasPrefix(line, "## ") {
			if cur != nil {
				sections = append(sections, *cur)
			}
			cur = &section{header: line}
			continue
		}
		if cur != nil {
			cur.body = append(cur.body, line)
		}
	}
	if cur != nil {
		sections = append(sections, *cur)
	}
	stats.Total = len(sections)

	// Process sections.
	out := make([]Attack, 0, len(sections))
	maxSeq := 0
	for _, sid := range priorAttackIDs {
		if m := idRE.FindStringSubmatch(sid); m != nil {
			if ci, _ := strconv.Atoi(m[1]); ci == criticIndex {
				if seq, _ := strconv.Atoi(m[2]); seq > maxSeq {
					maxSeq = seq
				}
			}
		}
	}
	seenIDs := map[string]bool{}

	for _, sec := range sections {
		m := sectionHeadRE.FindStringSubmatch(sec.header)
		if m == nil {
			stats.DroppedMalformedHeader++
			continue
		}
		id, location, dispTag := m[1], m[2], m[3]
		originalID := id
		bodyText := strings.Join(sec.body, "\n")

		// Disposition.
		var disp Disposition
		switch dispTag {
		case "":
			disp = DispIntroduce
		case "re-attack":
			disp = DispReAttack
		case "withdraw":
			disp = DispWithdraw
		default:
			disp = DispIntroduce
		}

		// Withdrawals are short-circuit: keep, no body filters.
		if disp == DispWithdraw {
			if !priorSet[id] {
				// Withdrawing an unknown id is a renaming → introduce.
				disp = DispIntroduce
			} else {
				out = append(out, Attack{
					AttackID: id, CriticIndex: criticIndex, Aspect: gotAspect,
					Round: round, Disposition: DispWithdraw, Location: location,
					WithdrawReason: extractField(bodyText, "reason"),
				})
				stats.KeptWithdraw++
				continue
			}
		}

		// Required-field checks.
		claim := extractField(bodyText, "claim")
		exp := extractField(bodyText, "expected violation")
		repro := extractFenced(bodyText, "reproduction")
		if repro == "" {
			stats.DroppedNoReproduce++
			continue
		}
		if !opt.AllowStyleAttacks && isStyleShaped(claim, exp) {
			stats.DroppedStyle++
			continue
		}
		if isCrossAspect(claim, expectedAspect) {
			stats.DroppedCrossAspect++
			continue
		}

		// Id normalization.
		idMatch := idRE.FindStringSubmatch(id)
		seq := 0
		if idMatch != nil {
			ci, _ := strconv.Atoi(idMatch[1])
			s, _ := strconv.Atoi(idMatch[2])
			if ci == criticIndex {
				seq = s
			}
		}
		if disp == DispReAttack {
			if !priorSet[id] {
				disp = DispIntroduce
				seq = 0
			}
		}
		if disp == DispIntroduce {
			// Reuse of a prior round's id without an explicit
			// (re-attack) or (withdraw) marker is treated as drift:
			// the section is kept (the claim might still be valid)
			// but renamed to a fresh id so the ledger does not
			// collapse two unrelated claims into one entry. Without
			// this, an R3 critic that emits "## c1-1 [...]" for a
			// brand-new flaw silently overwrites R1's c1-1.
			if seq == 0 || seenIDs[id] || idMatch == nil || priorSet[id] {
				maxSeq++
				seq = maxSeq
				id = fmt.Sprintf("c%d-%d", criticIndex, seq)
			} else if seq > maxSeq {
				maxSeq = seq
			}
		}
		// Count one rename per section: the id the critic wrote was
		// normalized to a different id. Counting the individual
		// normalization steps above double-counted a single section.
		if id != originalID {
			stats.Renamed++
		}
		seenIDs[id] = true

		a := Attack{
			AttackID: id, CriticIndex: criticIndex, Aspect: gotAspect,
			Round: round, Disposition: disp, Location: location,
			Claim: claim, ExpectedViolation: exp, Reproduction: repro,
		}
		if disp == DispIntroduce {
			a.RoundIntroduced = round
			stats.KeptIntroduce++
		} else {
			stats.KeptReAttack++
		}
		out = append(out, a)
	}

	return out, stats, nil
}

// Render is the canonical writer; the inverse of Parse.
func Render(criticIndex, round int, aspect string, attacks []Attack) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# Critic %d - round %d attacks\n\n", criticIndex, round)
	fmt.Fprintf(&b, "aspect: %s\n\n", aspect)
	for i, a := range attacks {
		var dispTag string
		switch a.Disposition {
		case DispReAttack:
			dispTag = " (re-attack)"
		case DispWithdraw:
			dispTag = " (withdraw)"
		}
		fmt.Fprintf(&b, "## %s [%s]%s\n\n", a.AttackID, a.Location, dispTag)
		switch a.Disposition {
		case DispWithdraw:
			fmt.Fprintf(&b, "reason: %s\n", a.WithdrawReason)
		default:
			fmt.Fprintf(&b, "claim: %s\n\n", a.Claim)
			fmt.Fprintf(&b, "expected violation: %s\n\n", a.ExpectedViolation)
			fmt.Fprintf(&b, "reproduction:\n```\n%s\n```\n", strings.TrimSpace(a.Reproduction))
		}
		if i < len(attacks)-1 {
			b.WriteString("\n---\n\n")
		}
	}
	return []byte(b.String())
}

func extractField(body, field string) string {
	field = strings.ToLower(field)
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		ll := strings.ToLower(strings.TrimSpace(l))
		if strings.HasPrefix(ll, field+":") {
			value := strings.TrimSpace(l[strings.Index(l, ":")+1:])
			// Single paragraph: continue lines until blank.
			for j := i + 1; j < len(lines); j++ {
				if strings.TrimSpace(lines[j]) == "" {
					break
				}
				value += "\n" + lines[j]
			}
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func extractFenced(body, label string) string {
	label = strings.ToLower(label)
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(l)), label+":") {
			rest := strings.Join(lines[i+1:], "\n")
			if m := fencePattern.FindStringSubmatch(rest); m != nil {
				return strings.TrimSpace(m[1])
			}
			return ""
		}
	}
	return ""
}

func isStyleShaped(claim, exp string) bool {
	if !anyMatch(stylePatterns, claim) {
		return false
	}
	if anyMatch(concretePatterns, exp) {
		return false
	}
	if fencePattern.MatchString(exp) {
		return false
	}
	return true
}

func anyMatch(patterns []*regexp.Regexp, s string) bool {
	for _, p := range patterns {
		if p.MatchString(s) {
			return true
		}
	}
	return false
}

func isCrossAspect(claim, expectedAspect string) bool {
	a := Lookup(expectedAspect)
	for _, kw := range a.ForbiddenKeywords {
		if strings.Contains(strings.ToLower(claim), strings.ToLower(kw)) {
			return true
		}
	}
	return false
}
