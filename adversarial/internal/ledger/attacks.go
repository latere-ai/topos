// Package ledger appends and aggregates attack-state transitions in
// attacks.jsonl. See specs/02-protocol.md.
package ledger

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"latere.ai/x/topos/adversarial/internal/state"
)

// Status enumerates the on-disk values for the attack state machine.
type Status string

// Status values.
const (
	// StatusOpen is the initial state when an attack is introduced.
	StatusOpen Status = "open"
	// StatusConceded is the proposer's concession + fix.
	StatusConceded Status = "conceded"
	// StatusRebutted is the proposer's rebuttal.
	StatusRebutted Status = "rebutted"
	// StatusWithdrawn is the critic's retraction.
	StatusWithdrawn Status = "withdrawn"
	// StatusUnresolved is the post-termination state for any open or
	// rebutted attack at run end.
	StatusUnresolved Status = "unresolved"
)

// SchemaAttack identifies the on-disk schema version.
const SchemaAttack = "adversarial.attack.v0"

// SpillThreshold is the inline-vs-spill cutoff per record (bytes).
const SpillThreshold = 64 * 1024

// Record is one transition for an attack_id; many records may share an id.
type Record struct {
	Schema            string    `json:"schema"`
	TS                time.Time `json:"ts"`
	AttackID          string    `json:"attack_id"`
	CriticIndex       int       `json:"critic_index"`
	Aspect            string    `json:"aspect"`
	RoundIntroduced   *int      `json:"round_introduced,omitempty"`
	Location          string    `json:"location,omitempty"`
	Claim             string    `json:"claim,omitempty"`
	ExpectedViolation string    `json:"expected_violation,omitempty"`
	Reproduction      string    `json:"reproduction,omitempty"`
	RoundLastTouched  int       `json:"round_last_touched"`
	Status            Status    `json:"status"`
	RoundsSurvived    int       `json:"rounds_survived"`
	ReAttacked        bool      `json:"re_attacked"`
	ConcessionFiles   []string  `json:"concession_files,omitempty"`
	IntroducedIn      string    `json:"introduced_in,omitempty"`
	LastTouchedIn     string    `json:"last_touched_in,omitempty"`
	BodyPath          string    `json:"body_path,omitempty"`
}

// Append writes one transition record to <session>/attacks.jsonl.
// When the inline body fields are too large, the bodies spill to a
// sidecar markdown file under forks/critic-<i>/attacks/<id>.md and the
// inline fields are blanked out on the JSONL line.
func Append(s *state.Session, r Record) error {
	if r.Schema == "" {
		r.Schema = SchemaAttack
	}
	if r.TS.IsZero() {
		r.TS = time.Now().UTC()
	}
	bodySize := len(r.Claim) + len(r.ExpectedViolation) + len(r.Reproduction)
	if bodySize > SpillThreshold {
		if err := spillBody(s, &r); err != nil {
			return err
		}
		r.Claim, r.ExpectedViolation, r.Reproduction = "", "", ""
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.AppendLine("attacks.jsonl", b)
}

// Aggregate reads attacks.jsonl forward and folds records by attack_id.
// Later non-zero/non-empty fields supersede earlier ones; final status
// is the last status seen. Any unparseable line is skipped: the ledger
// is append-only, so the only realistic corruption is a truncated final
// line from a crash mid-write, and best-effort aggregation is preferred
// over failing a run on a single bad line.
func Aggregate(s *state.Session) (map[string]Record, error) {
	path := s.Path("attacks.jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]Record{}, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	out := map[string]Record{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			// Tolerate any unparseable line (see Aggregate's doc): skip it
			// and keep folding the rest rather than failing the run.
			continue
		}
		fold(out, r)
	}
	return out, nil
}

// LoadBody resolves the body_path side-car if present; otherwise returns r.
func LoadBody(s *state.Session, r Record) (Record, error) {
	if r.BodyPath == "" {
		return r, nil
	}
	b, err := os.ReadFile(s.Path(r.BodyPath))
	if err != nil {
		return r, err
	}
	r.Claim, r.ExpectedViolation, r.Reproduction = parseSpill(string(b))
	return r, nil
}

// Pending returns records whose Status is open or rebutted, sorted by
// AttackID for determinism.
func Pending(agg map[string]Record) []Record {
	out := make([]Record, 0)
	for _, r := range agg {
		if r.Status == StatusOpen || r.Status == StatusRebutted {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AttackID < out[j].AttackID })
	return out
}

func fold(out map[string]Record, in Record) {
	cur, ok := out[in.AttackID]
	if !ok {
		out[in.AttackID] = in
		return
	}
	if in.RoundIntroduced != nil {
		cur.RoundIntroduced = in.RoundIntroduced
	}
	if in.Location != "" {
		cur.Location = in.Location
	}
	if in.Claim != "" {
		cur.Claim = in.Claim
	}
	if in.ExpectedViolation != "" {
		cur.ExpectedViolation = in.ExpectedViolation
	}
	if in.Reproduction != "" {
		cur.Reproduction = in.Reproduction
	}
	if in.IntroducedIn != "" {
		cur.IntroducedIn = in.IntroducedIn
	}
	if in.RoundLastTouched > 0 {
		cur.RoundLastTouched = in.RoundLastTouched
	}
	if in.LastTouchedIn != "" {
		cur.LastTouchedIn = in.LastTouchedIn
	}
	if in.Status != "" {
		cur.Status = in.Status
	}
	if in.RoundsSurvived > cur.RoundsSurvived {
		cur.RoundsSurvived = in.RoundsSurvived
	}
	if in.ReAttacked {
		cur.ReAttacked = true
	}
	if len(in.ConcessionFiles) > 0 {
		cur.ConcessionFiles = in.ConcessionFiles
	}
	if in.BodyPath != "" {
		cur.BodyPath = in.BodyPath
	}
	cur.TS = in.TS
	cur.Aspect = in.Aspect
	cur.CriticIndex = in.CriticIndex
	out[in.AttackID] = cur
}

// spillDoc is the JSON side-car shape for a spilled record body. JSON
// is used instead of "## "-delimited markdown because the body fields
// are LLM-generated and can themselves contain "## " lines, which the
// old markdown parser mistook for section boundaries and silently
// truncated on round-trip.
type spillDoc struct {
	Claim             string `json:"claim"`
	ExpectedViolation string `json:"expected_violation"`
	Reproduction      string `json:"reproduction"`
}

func spillBody(s *state.Session, r *Record) error {
	rel := filepath.Join("forks", fmt.Sprintf("critic-%d", r.CriticIndex), "attacks", r.AttackID+".json")
	body, err := json.Marshal(spillDoc{
		Claim: r.Claim, ExpectedViolation: r.ExpectedViolation, Reproduction: r.Reproduction,
	})
	if err != nil {
		return err
	}
	if err := s.AtomicWrite(rel, body); err != nil {
		return err
	}
	r.BodyPath = rel
	return nil
}

func parseSpill(b string) (claim, exp, repro string) {
	var d spillDoc
	if err := json.Unmarshal([]byte(b), &d); err == nil {
		return d.Claim, d.ExpectedViolation, d.Reproduction
	}
	// Backward-compat: older side-cars used "## "-delimited markdown.
	sections := splitOnHeader(b, "## ")
	for _, sec := range sections {
		switch {
		case startsWith(sec, "Claim"):
			claim = trimSection(sec, "Claim")
		case startsWith(sec, "Expected violation"):
			exp = trimSection(sec, "Expected violation")
		case startsWith(sec, "Reproduction"):
			repro = trimSection(sec, "Reproduction")
		}
	}
	return claim, exp, repro
}

func splitOnHeader(s, sep string) []string {
	var out []string
	cur := ""
	for _, line := range splitLines(s) {
		if startsWith(line, sep) {
			if cur != "" {
				out = append(out, cur)
			}
			cur = line[len(sep):] + "\n"
		} else if cur != "" {
			cur += line + "\n"
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if c == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func trimSection(sec, header string) string {
	// strip header line, leading blanks
	for i := len(header); i < len(sec); i++ {
		if sec[i] != '\n' {
			continue
		}
		rest := sec[i+1:]
		// trim leading newlines
		for len(rest) > 0 && rest[0] == '\n' {
			rest = rest[1:]
		}
		// trim trailing newlines
		for len(rest) > 0 && rest[len(rest)-1] == '\n' {
			rest = rest[:len(rest)-1]
		}
		return rest
	}
	return ""
}
