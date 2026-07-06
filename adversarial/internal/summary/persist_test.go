package summary

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/adversarial/internal/ledger"
	"latere.ai/x/topos/adversarial/internal/round"
	"latere.ai/x/topos/adversarial/internal/state"
)

func TestPersistWritesSummaryAndEnd(t *testing.T) {
	sess, err := state.NewSession(t.TempDir(), 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sumRes := &round.Summary{
		Sess: sess, Termination: round.TermSteadyState,
		Forks:      []round.ForkOutcome{{Index: 1, Topic: "security", Rounds: 4, Termination: round.TermSteadyState}},
		TokensUsed: 1234, WallSeconds: 7,
		Unresolved: 1,
	}
	agg := map[string]ledger.Record{
		"c1-1": {
			AttackID: "c1-1", Aspect: "security", Status: ledger.StatusUnresolved,
			Location: "x.go:1", Claim: "leak", ExpectedViolation: "panic",
			Reproduction: "go run", RoundIntroduced: ptr(1), RoundLastTouched: 3, ReAttacked: true,
		},
		"c1-2": {
			AttackID: "c1-2", Aspect: "security", Status: ledger.StatusConceded,
			Claim: "off-by-one", ConcessionFiles: []string{"x.go"},
		},
		"c1-3": {
			AttackID: "c1-3", Aspect: "security", Status: ledger.StatusUnresolved,
			Location: "y.go:1", Claim: "second issue", Reproduction: "ok",
			RoundIntroduced: ptr(1), RoundLastTouched: 2, ReAttacked: false,
		},
	}
	if err := Persist(sumRes, agg, 1); err != nil {
		t.Fatal(err)
	}

	sum, err := os.ReadFile(sess.Path("summary.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(sum)
	for _, want := range []string{
		"## Headline", "## Other unresolved", "## Resolved", "## Stats",
		"critic-found-bug rate: 1/3",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("summary missing %q:\n%s", want, body)
		}
	}

	end, err := os.ReadFile(sess.Path("end.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ef state.EndFile
	if err := json.Unmarshal(end, &ef); err != nil {
		t.Fatal(err)
	}
	if ef.ExitCode != 1 {
		t.Errorf("ExitCode: got %d", ef.ExitCode)
	}
	if ef.Headline == nil || ef.Headline.AttackID != "c1-1" {
		t.Errorf("headline: %+v", ef.Headline)
	}
	if ef.Stats.ByStatus["conceded"] != 1 || ef.Stats.ByStatus["unresolved"] != 2 {
		t.Errorf("status counts: %+v", ef.Stats.ByStatus)
	}
}

func TestRenderJSONFormatPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for json format")
		}
	}()
	r := &Render{Format: "json"}
	_, _ = r.Bytes(&round.Summary{Termination: round.TermSteadyState}, map[string]ledger.Record{})
}
