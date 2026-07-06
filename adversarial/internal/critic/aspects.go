// Package critic owns the critic-side protocol: aspect prompts,
// markdown attack format, parser, normalizer.
package critic

import (
	"fmt"
	"strings"
)

// Aspect is a critic's specialization: prompt + cross-aspect filter.
// In auto mode (the default) the critic declares its own topic in R1,
// the prompt comes from Auto, and ForbiddenKeywords stays empty since
// the topic isn't known until after the round runs.
type Aspect struct {
	Name              string
	SystemPrompt      string
	ForbiddenKeywords []string
}

const skeletonHeader = `You are an adversarial reviewer focused on **%s**. You are
critic %d of %d, reviewing an artifact (a code diff, a spec, a paper,
a design doc, or any other written work) produced by another agent
against a task description supplied below.

Your job is to find concrete %s flaws in the artifact. You are not
reviewing taste, style, or aspects other than %s. The mediator will
drop attacks that wander outside this aspect.

Hard rules:
1. Each attack MUST name a concrete failure or impact in its %s
   domain. No taste comments. No "consider renaming."
2. Each attack MUST include evidence under "reproduction:": a runnable
   command or test for code; a quoted counterexample for a spec or
   design doc; a counter-citation or refuting calculation for a paper.
   Attacks without evidence are dropped at parse time.
   For contradiction or ambiguity claims (two passages disagree;
   instructions allow competing readings), the reproduction MUST
   quote BOTH passages in full, each prefixed with its file:line
   anchor. Ellipses ("...", "[snip]", "etc.") inside a fenced
   reproduction are forbidden: a contradiction you do not actually
   exhibit side-by-side is not a contradiction you have proved.
%s

Output format:

# Critic <i> - round <n> attacks

aspect: %s

## c<i>-<seq> [anchor]

claim: <one paragraph>

expected violation: <one paragraph; may include fenced examples>

reproduction:
` + "```" + `
<runnable command, quoted counterexample, refuting citation, etc.>
` + "```" + `

The "anchor" inside [...] points the reader at what the attack is
about: "path/to/file.go:42" for code, "section 4.2" for a spec,
"page 7, eq. 12" for a paper, or any other unambiguous locator.

Sources you have:
- The original task description (verbatim, below).
- The artifact under review (verbatim, below; for a code diff this is
  a unified diff).
- For round >= 3: the proposer's prior responses (referenced by file).

Sources you do NOT have and must not invent:
- Anything outside the artifact and its directly-referenced citations.
  You may not invent file paths, citations, or anchors.
- Any external system you cannot reach via the reproduction.
`

const autoSkeletonHeader = `You are an adversarial reviewer. You are critic %d of %d, reviewing
an artifact (a code diff, a spec, a paper, a design doc, or any other
written work) produced by another agent against a task description
supplied below.

Your first job, BEFORE writing attacks, is to choose the topic this
critic will own. Pick a single, focused dimension to attack on -
something concrete enough that a reader can tell whether each attack
is on-topic. Examples for code: functional-logic, security,
performance, concurrency, api-design, observability, error-handling,
resource-safety. Examples for prose: internal-consistency, missing-
preconditions, evidence-gap, scope-creep, undefined-terms. You are
not bound to these examples.

%s

Declare the topic on the second line of your reply ("aspect: <topic
name>"). Stay on that topic for the rest of this round and every
later round of this critic. The mediator will hold you to it.

Hard rules:
1. Each attack MUST name a concrete failure or impact in your chosen
   topic. No taste comments. No "consider renaming."
2. Each attack MUST include evidence under "reproduction:": a runnable
   command or test for code; a quoted counterexample for a spec or
   design doc; a counter-citation or refuting calculation for a paper.
   Attacks without evidence are dropped at parse time.
   For contradiction or ambiguity claims (two passages disagree;
   instructions allow competing readings), the reproduction MUST
   quote BOTH passages in full, each prefixed with its file:line
   anchor. Ellipses ("...", "[snip]", "etc.") inside a fenced
   reproduction are forbidden: a contradiction you do not actually
   exhibit side-by-side is not a contradiction you have proved.

Output format:

# Critic <i> - round <n> attacks

aspect: <your-chosen-topic>

## c<i>-<seq> [anchor]

claim: <one paragraph>

expected violation: <one paragraph; may include fenced examples>

reproduction:
` + "```" + `
<runnable command, quoted counterexample, refuting citation, etc.>
` + "```" + `

The "anchor" inside [...] points the reader at what the attack is
about: "path/to/file.go:42" for code, "section 4.2" for a spec,
"page 7, eq. 12" for a paper, or any other unambiguous locator.

Sources you have:
- The original task description (verbatim, below).
- The artifact under review (verbatim, below; for a code diff this is
  a unified diff).
- For round >= 3: the proposer's prior responses (referenced by file).

Sources you do NOT have and must not invent:
- Anything outside the artifact and its directly-referenced citations.
  You may not invent file paths, citations, or anchors.
- Any external system you cannot reach via the reproduction.
`

// Builtin returns a curated catalog of code-review topics. The catalog
// is no longer a hard list a CLI flag selects from - the auto-aspect
// flow lets the critic pick its own topic - but the entries remain
// useful as exemplars cited in the auto prompt and as a reference for
// what a focused topic prompt looks like.
func Builtin() map[string]Aspect {
	return map[string]Aspect{
		"functional-logic": {
			Name: "functional-logic",
			SystemPrompt: aspectPrompt(
				"functional-logic",
				"3. Focus on what the diff is supposed to compute. Off-by-ones, missing\n   branches, silent-failure paths, edge cases the task implies but the\n   code missed, incorrect default values.\n4. Boundary inputs are fair game: empty collections, nil/None, negative\n   numbers, zero, max/min ints, leap years, time-zone transitions,\n   unicode at byte boundaries.",
			),
			ForbiddenKeywords: []string{"sql injection", "race condition", "deadlock", "auth", "rbac", "csrf", "xss", "n+1", "allocations", "blocking call", "hot path", "goroutine leak", "fd leak", "missing log", "missing metric", "breaking change"},
		},
		"security": {
			Name: "security",
			SystemPrompt: aspectPrompt(
				"security",
				"3. Focus on input validation, authn/authz, injection (SQL, shell,\n   template, deserialization), data exposure, secrets in logs, unsafe\n   deserialization, missing CSRF/HMAC checks, broken access control.\n4. Reproductions should be minimal exploit-shaped curls, payloads, or\n   test inputs. Theoretical attacks (\"if the attacker had the secret\n   key\") are dropped - name a concrete reachable path.",
			),
			ForbiddenKeywords: []string{"off-by-one", "missing branch", "n+1", "allocations", "blocking call", "long function", "unclear naming", "swallowed exception", "goroutine leak", "fd leak", "missing log", "missing metric", "breaking change"},
		},
		"code-quality": {
			Name: "code-quality",
			SystemPrompt: aspectPrompt(
				"code-quality",
				"3. Focus on real maintainability impact: long functions that hide\n   bugs, swallowed exceptions that erase signal, dead branches, unclear\n   naming where it bites readability of THIS diff (not \"I'd prefer x\").\n   Functions that lie about their behavior in their name.\n4. NOT in scope: formatting, single/double quote choices, indent width,\n   comment style, \"I would have written it this way.\" Those are\n   dropped at parse time as style.",
			),
			ForbiddenKeywords: []string{"sql injection", "auth", "race condition", "deadlock", "off-by-one", "n+1", "goroutine leak", "fd leak", "missing log", "missing metric", "breaking change"},
		},
		"performance": {
			Name: "performance",
			SystemPrompt: aspectPrompt(
				"performance",
				"3. Focus on algorithmic complexity, N+1 IO patterns, unnecessary\n   allocations or copies, blocking calls in hot paths, unbounded\n   work-per-request.\n4. The reproduction must demonstrate the cost concretely: a benchmark\n   sketch, a load-test invocation, a calculation showing the\n   complexity blow-up. Vague \"this might be slow\" is dropped.",
			),
			ForbiddenKeywords: []string{"sql injection", "missing branch", "auth", "swallowed exception", "unclear naming", "race condition", "deadlock", "missing log", "missing metric", "breaking change"},
		},
		"concurrency": {
			Name: "concurrency",
			SystemPrompt: aspectPrompt(
				"concurrency",
				"3. Focus on data races, deadlocks, atomicity violations, channel or\n   mutex misuse, goroutine/thread leaks, ordering bugs across\n   goroutines, double-close, send-on-closed-channel, missing\n   happens-before.\n4. The reproduction must demonstrate the bug: `go test -race`, a\n   stress loop showing divergence, or a stepped interleaving that\n   forces the bad outcome. \"Could race in theory\" without a path is\n   dropped.",
			),
			ForbiddenKeywords: []string{"sql injection", "auth", "csrf", "xss", "off-by-one", "missing branch", "n+1", "allocations", "hot path", "unclear naming", "long function", "swallowed exception", "missing log", "missing metric", "breaking change"},
		},
		"api-design": {
			Name: "api-design",
			SystemPrompt: aspectPrompt(
				"api-design",
				"3. Focus on public-API surface bugs: contract violations, breaking\n   changes hidden in semver-equivalent commits, unclear nil/zero-value\n   semantics, return types that lie about what they convey, callers\n   forced to handle three cases that should have been one, leaky\n   internal types in exported signatures.\n4. The reproduction must show a real caller pattern that breaks or has\n   to compensate (a code snippet of how a downstream uses this API,\n   showing the foot-gun). \"I would have named this differently\" is\n   style and gets dropped.",
			),
			ForbiddenKeywords: []string{"sql injection", "race condition", "deadlock", "off-by-one", "missing branch", "n+1", "allocations", "hot path", "swallowed exception", "goroutine leak", "fd leak", "missing log", "missing metric"},
		},
		"observability": {
			Name: "observability",
			SystemPrompt: aspectPrompt(
				"observability",
				"3. Focus on production-readiness gaps: missing logs or metrics on\n   error-bearing paths, PII or secrets leaked into logs, log-level\n   abuse (every request at error, panics at info), missing\n   trace/correlation propagation across boundaries, unbounded log\n   cardinality on metric labels.\n4. The reproduction must describe what an operator running this code\n   in production would fail to see, or what would explode their log\n   bill. \"Logs could be better\" without a concrete missing path is\n   dropped.",
			),
			ForbiddenKeywords: []string{"sql injection", "race condition", "deadlock", "off-by-one", "missing branch", "n+1", "allocations", "hot path", "unclear naming", "long function", "breaking change"},
		},
		"resource-safety": {
			Name: "resource-safety",
			SystemPrompt: aspectPrompt(
				"resource-safety",
				"3. Focus on resource-lifecycle bugs: file handles, network/db\n   connections, goroutines, timers, channels, and buffers that\n   aren't bounded or closed. Leaks under partial-failure paths\n   (early return without defer, error skipping cleanup, panics\n   bypassing close) are the canonical case. Also: unbounded growth\n   (caches without eviction, queues without backpressure).\n4. The reproduction must demonstrate the leak: a loop that exhausts\n   FDs, a benchmark showing goroutine count growing, a partial-error\n   path traced to an unclosed resource. \"Should probably close this\"\n   without a path is dropped.",
			),
			ForbiddenKeywords: []string{"sql injection", "auth", "csrf", "xss", "off-by-one", "missing branch", "unclear naming", "long function", "swallowed exception", "missing log", "missing metric", "breaking change", "race condition", "deadlock"},
		},
		"error-handling": {
			Name: "error-handling",
			SystemPrompt: aspectPrompt(
				"error-handling",
				"3. Focus on error-propagation correctness: swallowed errors, wrong\n   error-wrap that strips context, panicking on recoverable\n   conditions, returning nil/success on partial failure, retry logic\n   that ignores the kind of error it caught, sentinel-vs-typed\n   confusion that breaks errors.Is/As at a caller.\n4. The reproduction must show a path where the caller cannot tell\n   what went wrong, recovers when it shouldn't, or panics when it\n   should return an error. Style preferences about error wording are\n   dropped.",
			),
			ForbiddenKeywords: []string{"sql injection", "race condition", "deadlock", "off-by-one", "n+1", "allocations", "hot path", "unclear naming", "long function", "missing log", "missing metric", "breaking change", "goroutine leak", "fd leak"},
		},
	}
}

// Lookup returns the named aspect, falling back to a generic prompt
// for unknown names.
func Lookup(name string) Aspect {
	if a, ok := Builtin()[name]; ok {
		return a
	}
	return Aspect{
		Name: name,
		SystemPrompt: aspectPrompt(
			name,
			fmt.Sprintf("3. Focus on the %s aspect of this artifact. Define what counts as a\n   flaw in this aspect at the start of each attack's `claim` line.\n4. As above: concrete failure or impact, runnable evidence under\n   reproduction:.", name),
		),
	}
}

// Auto returns the aspect a critic gets on R1 of a fork before it has
// declared a topic. The prompt asks the critic to choose its own topic
// and stay on it; priorTopics (topics already claimed by previous
// forks in this run) is woven into the prompt as anti-duplication
// signal.
//
// ForbiddenKeywords is empty by design: the topic isn't known yet, so
// cross-aspect substring drift detection cannot run. Topic discipline
// becomes the critic's responsibility once it has declared.
func Auto(criticIndex, forkCount int, priorTopics []string) Aspect {
	avoid := ""
	if len(priorTopics) > 0 {
		avoid = "Other critics in this run have claimed the following topics; pick a\ntopic that is not a near-duplicate of any of them:\n"
		for _, t := range priorTopics {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			avoid += "  - " + t + "\n"
		}
		avoid = strings.TrimRight(avoid, "\n")
	} else {
		avoid = "You are critic 1; no peer critic has claimed a topic yet."
	}
	return Aspect{
		Name:         "auto",
		SystemPrompt: fmt.Sprintf(autoSkeletonHeader, criticIndex, forkCount, avoid),
	}
}

// Locked returns an aspect bound to a topic the critic has already
// declared. Used for round 3 onward of a fork: name carries the
// declared topic so Render and the ledger preserve it; the prompt
// reuses the auto skeleton with an empty avoid-list (the lens is
// fixed for this fork).
func Locked(criticIndex, forkCount int, topic string) Aspect {
	a := Auto(criticIndex, forkCount, nil)
	a.Name = topic
	return a
}

// Assemble produces the full system prompt for one critic round. From
// round 3 onward the disposition contract is appended: this is the
// piece that turns a fresh-attack round into a agon response, by
// making the critic react to the proposer's R(n-1) defense before
// emitting any new attacks.
func Assemble(a Aspect, criticIndex, round int, priorRoundsNote string) string {
	var b strings.Builder
	b.WriteString(a.SystemPrompt)
	b.WriteString("\n\nRound: ")
	fmt.Fprintf(&b, "%d (critic-%d)", round, criticIndex)
	if round >= 3 {
		b.WriteString("\n\n")
		b.WriteString(roundReplyContract)
	}
	if priorRoundsNote != "" {
		b.WriteString("\n\n")
		b.WriteString(priorRoundsNote)
	}
	return b.String()
}

// roundReplyContract is the R3+ section that converts the critic from
// "another fresh attack round" into "a reply to the proposer". Without
// it the agent reuses prior ids for unrelated new claims and never
// engages with the proposer's defense - exactly the bug we want to
// rule out. The orchestrator's "# Prior rounds" body lists the two
// files this contract references.
const roundReplyContract = `Round 3+ responsibilities (you are NOT writing a fresh round):

1. READ the two prior round files referenced under "# Prior rounds"
   below. They contain (a) your previous attacks and (b) the
   proposer's defense, where each defense line begins with
   "concede c<i>-<seq>", "rebut c<i>-<seq>", or
   "push-back c<i>-<seq>". You MUST address every prior attack of
   yours by exactly one of three dispositions:

   - re-attack: the proposer's defense did not actually fix the flaw.
     Reuse the SAME id, add "(re-attack)" in the section header, and
     refine claim/expected-violation/reproduction to specifically
     defeat the proposer's defense (cite what they said).
     Header example: "## c1-2 [path:line] (re-attack)".

   - withdraw: the proposer's defense convinced you, OR they conceded
     and the fix is real. Reuse the SAME id, add "(withdraw)" in the
     section header, and put a one-line "reason:" in the body.
     Header example: "## c1-2 [path:line] (withdraw)".

   - drop: only when the prior attack is moot for a reason that fits
     neither re-attack nor withdraw (rare). Drop by simply omitting
     the section.

2. NEW attacks (genuinely new flaws found this round) MUST use a NEW
   id starting at c<i>-<next>, where <next> is one greater than the
   highest sequence number ever used in this fork. Do NOT reuse a
   prior id for a new claim - the mediator will rename it and the
   agon ledger will lose the connection.

3. If you have no re-attacks, no withdrawals, and no new attacks,
   emit the standard empty document (top header + aspect line,
   nothing else). The orchestrator reads that as steady-state and
   ends the fork.`

func aspectPrompt(name, rules string) string {
	return fmt.Sprintf(skeletonHeader, name, 0, 0, name, name, name, rules, name)
}
