# Sessions

`Runner.Run` takes a task and runs a region to the end. A chat assistant, or a
coding agent steered one prompt at a time, needs the opposite shape: a
conversation that stops after each answer and continues later, perhaps on
another machine. `Runner.Turn` is that shape.

A turn runs one agent, once, against a sandbox the host created and keeps.
It takes the conversation so far and returns the conversation with the new
turn appended. The runtime holds no session state between calls: the
transcript the host stores is the whole of it.

## Driving a session

```go
import (
	"latere.ai/x/topos"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/sandbox"
	"latere.ai/x/topos/sandbox/local"
)

// The host creates the sandbox and keeps it for the whole session, so files
// and installed dependencies survive between turns.
prov := local.New()
box, err := prov.Create(ctx, sandbox.CreateOptions{})
if err != nil {
	return err
}
defer prov.Destroy(context.WithoutCancel(ctx), box.ID)

r, err := topos.NewRunner(topos.Options{
	SessionID: "sess-42",
	Model:     topos.ModelOptions{Kind: topos.ModelLux, APIKey: luxKey},
})
if err != nil {
	return err
}

var transcript []models.Message // load it from storage to resume a session
for _, prompt := range []string{"add a test for parse()", "now make it pass"} {
	res, err := r.Turn(ctx, topos.TurnInput{
		Sandbox:           prov,
		SandboxID:         box.ID,
		SystemPrompt:      "You are a careful Go engineer.",
		InitialTranscript: transcript,
		UserPrompt:        prompt,
	})
	if err != nil {
		return err
	}
	transcript = res.Transcript // persist this: it is the session
	fmt.Println(res.Final)
}
```

`Turn` neither creates nor destroys the sandbox, and it runs exactly one agent:
there is no delegation inside a turn. `TurnInput.Tools` replaces the builtin
tool set with a registry the host builds, for example to add tools of its own;
nil offers every builtin. `TurnInput.MaxTokens` caps the length of one
response, and zero leaves the provider's default.

`TurnResult` carries the full `Transcript`, the turn's `Final` text, the
model's `StopReason`, the turn's token `Usage`, and the number of `ToolCalls`
it executed. An empty `UserPrompt` continues without new input, which is how a
host resumes a turn that was interrupted.

## Interrupting a turn

Cancelling the context interrupts the turn, which is what a host does when a
person presses Escape. `Turn` then returns the conversation up to the cut,
including any partial assistant text, with `Interrupted` set and a nil error.
An interrupt is a normal action, not a failure, so the transcript is kept and
the session goes on from it. Tool calls the model requested but the runtime
never executed are left out, so the transcript stays valid to resume.

A failure of the model or the sandbox is a non-nil error, returned together
with whatever part of the transcript was captured.

## Observing a run

`Options.Observer` receives every event a run or a turn emits, in the order
they happen. It is the way to render tokens as they stream, show tool calls,
draw a live trace, or display token usage.

```go
events := make(chan topos.Event, 256)
r, err := topos.NewRunner(topos.Options{
	Model:    topos.ModelOptions{Kind: topos.ModelLux, APIKey: luxKey},
	Observer: func(e topos.Event) { events <- e },
})
```

Each `Event` has a `Name`, the `SessionID` of the agent that emitted it, the
agent's name in `AgentID` when the payload carries one, a UTC timestamp, and
the full payload as JSON in `PayloadJSON`. For an agent in `Run` or
`RunGraph`, `SessionID` equals the id of that agent's trace node, so events
join to the trace.

The constants name the events a host usually switches on:

| Constant | When it is emitted |
|---|---|
| `EventSessionStart`, `EventSessionEnd` | an agent's loop starts and ends |
| `EventUserPromptSubmit` | a prompt enters the conversation |
| `EventTextDelta` | a fragment of assistant text arrives from the model; many per turn |
| `EventAssistantMessage` | the assembled assistant message of a turn |
| `EventPostToolUse` | a tool call finished |
| `EventUsage` | a turn finished; the payload has the turn's usage and the running total |
| `EventSubagentStart`, `EventSubagentStop` | a delegated peer starts and stops |
| `EventStop` | an agent's loop finished; the payload has the stop reason and the number of tool calls |

The observer also receives `PreToolUse` before a tool call runs and
`PostToolUseFailure` when one fails. The root package has no constants for
those two; compare `Name` with the string.

A `TextDelta` payload carries the fragment in `text` and the 1-based turn it
belongs to in `turn`, so a host can group fragments under the assistant
message that follows them.

The observer only watches: its return value cannot change what the run does.
It is called synchronously on the goroutine that emitted the event, so a slow
observer slows the run. Hand the event to a buffered channel, as above, and
return. A panic inside the observer is recovered and does not stop the run.
Events from different agents can interleave, so route them by `SessionID`
rather than by arrival order.

## Budgets across a session

With `Options.BudgetUSD` set, each `Turn` is metered on its own: the cap
applies per turn, not across the conversation. A turn stopped by the cap
returns its partial transcript, `StopReason` set to
`models.StopBudgetExceeded`, and an error matching `billing.ErrBudgetExceeded`.
Unlike an interrupt, that is an error, because nobody asked for the stop and a
truncated answer must not read as a complete one. [Models and
budgets](models.md) describes how a turn is priced.
