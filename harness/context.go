// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"
	"latere.ai/x/pkg/llmdialect/tokencount"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// Context management constants of spec 010.
const (
	DefaultCompactAt = 0.8
	// keepResults is how many of the thread's most recent steps keep
	// their tool results when the context is cleared.
	keepResults = 10
	// keepSteps is how many of the most recent steps a summary leaves
	// verbatim.
	keepSteps = 3
)

// The error codes of spec 010.
const (
	CodeCompactionFailed = "compaction_failed"
	CodeContextExhausted = "context_exhausted"
)

// promptTokens is a response's prompt tokens: input plus the cache reads
// and writes the IR counts apart.
func promptTokens(u lux.Usage) int64 {
	n := u.InputTokens
	for _, c := range []*int64{u.CacheReadInputTokens, u.CacheWriteInputTokens} {
		if c != nil {
			n += *c
		}
	}
	return n
}

// estimate is the next request's prompt tokens: the counter's estimate,
// corrected by how far the provider's count was from the counter on the
// last request.
func (t *turn) estimate(req ir.Request) int64 {
	return max(tokencount.Estimate(&req)+t.bias, 0)
}

func (t *turn) threshold() int64 {
	at := t.h.c.CompactAt
	if at == 0 {
		at = DefaultCompactAt
	}
	at = min(max(at, 0.5), 0.95)
	return int64(float64(t.h.c.Entry.InputWindow) * at)
}

// manageContext keeps the request under the threshold (spec 010): it
// clears old tool results first, then compacts, and fails the turn with
// context_exhausted when the request is still over the window.
func (t *turn) manageContext(ctx context.Context, req ir.Request, toolsSHA string) (ir.Request, string, error) {
	limit := t.threshold()
	est := t.estimate(req)
	if est < limit {
		return req, toolsSHA, nil
	}
	cleared, err := t.clear(ctx, est)
	if err != nil {
		return ir.Request{}, "", err
	}
	if cleared {
		if req, toolsSHA, est, err = t.rebuild(ctx); err != nil {
			return ir.Request{}, "", err
		}
	}
	if est >= limit {
		compacted, err := t.compact(ctx, est)
		if err != nil {
			return ir.Request{}, "", err
		}
		if compacted {
			if req, toolsSHA, est, err = t.rebuild(ctx); err != nil {
				return ir.Request{}, "", err
			}
		}
	}
	if est > t.h.c.Entry.InputWindow {
		se, err := t.sessionError(CodeContextExhausted, fmt.Sprintf("the request is %d tokens after compaction; the model's window is %d", est, t.h.c.Entry.InputWindow), false, "")
		if err != nil {
			return ir.Request{}, "", err
		}
		return ir.Request{}, "", t.finish(ctx, session.StopError, CodeContextExhausted, se)
	}
	return req, toolsSHA, nil
}

// rebuild builds the request again from the log as it is now, after a
// clear or a compaction appended to it.
func (t *turn) rebuild(ctx context.Context) (ir.Request, string, int64, error) {
	evs := t.events()
	tr, err := session.Fold(evs, t.thread)
	if err != nil {
		return ir.Request{}, "", 0, err
	}
	if len(evs) > 0 {
		t.seen = evs[len(evs)-1].Seq
	}
	req, sum, err := t.request(ctx, tr)
	if err != nil {
		return ir.Request{}, "", 0, err
	}
	return req, sum, t.estimate(req), nil
}

// steps are the thread's agent.message events in order: one per step.
func (t *turn) steps() []session.Event {
	var out []session.Event
	for _, e := range t.events() {
		if e.Type == session.TypeAgentMessage && e.Thread == t.thread && !e.Redacted() {
			out = append(out, e)
		}
	}
	return out
}

// clear appends a clear_tool_results compaction for every result older
// than the thread's last ten steps, results already cleared aside, and
// the results of todo and of question, which are never cleared: the
// cleared text tells the model to run the tool again, which for a
// question would ask the person twice (spec 039). It reports false when
// there is nothing to clear.
func (t *turn) clear(ctx context.Context, before int64) (bool, error) {
	steps := t.steps()
	if len(steps) <= keepResults {
		return false, nil
	}
	keepFrom := steps[len(steps)-keepResults].Seq
	names := map[string]string{}
	done := map[string]bool{}
	for _, e := range t.events() {
		if e.Thread != t.thread || e.Redacted() {
			continue
		}
		switch e.Type {
		case session.TypeAgentToolUse:
			var p session.AgentToolUse
			if e.Decode(&p) == nil {
				names[p.ToolUseID] = p.Name
			}
		case session.TypeContextCompacted:
			var p session.ContextCompacted
			if e.Decode(&p) == nil && p.Kind == session.CompactClearToolResults {
				for _, id := range p.ToolUseIDs {
					done[id] = true
				}
			}
		}
	}
	var ids []string
	var from, to uint64
	for _, e := range t.events() {
		if e.Seq >= keepFrom || e.Thread != t.thread || e.Redacted() {
			continue
		}
		var id string
		switch e.Type {
		case session.TypeToolResult:
			var p session.ToolResult
			if e.Decode(&p) != nil {
				continue
			}
			id = p.ToolUseID
		case session.TypeUserToolResult:
			var p session.UserToolResult
			if e.Decode(&p) != nil {
				continue
			}
			id = p.ToolUseID
		default:
			continue
		}
		if done[id] || names[id] == "todo" || names[id] == ToolQuestion || slices.Contains(ids, id) {
			continue
		}
		ids = append(ids, id)
		if from == 0 {
			from = e.Seq
		}
		to = e.Seq
	}
	if len(ids) == 0 {
		return false, nil
	}
	c := session.ContextCompacted{Kind: session.CompactClearToolResults, FromSeq: from, ToSeq: to, ToolUseIDs: ids, Cause: session.CauseThreshold, TokensBefore: before}
	e, err := t.event(session.TypeContextCompacted, c)
	if err != nil {
		return false, err
	}
	after, err := t.estimateWith(ctx, e)
	if err != nil {
		return false, err
	}
	c.TokensAfter = after
	if e, err = t.event(session.TypeContextCompacted, c); err != nil {
		return false, err
	}
	return true, t.commit(ctx, e)
}

// estimateWith estimates the request the log would give with evs
// appended, without appending them.
func (t *turn) estimateWith(ctx context.Context, evs ...session.Event) (int64, error) {
	log := slices.Clone(t.events())
	next := uint64(1)
	if n := len(log); n > 0 {
		next = log[n-1].Seq + 1
	}
	for _, e := range evs {
		e.Seq = next
		next++
		log = append(log, e)
	}
	tr, err := session.Fold(log, t.thread)
	if err != nil {
		return 0, err
	}
	req, _, err := t.request(ctx, tr)
	if err != nil {
		return 0, err
	}
	return t.estimate(req), nil
}

// compact asks the thread's own model to summarize everything before
// its three most recent steps, and appends the summary compaction. It
// reports false when there are too few steps to summarize.
func (t *turn) compact(ctx context.Context, before int64) (bool, error) {
	steps := t.steps()
	if len(steps) <= keepSteps {
		return false, nil
	}
	return t.summarize(ctx, before, steps[len(steps)-keepSteps].Seq-1, session.CauseThreshold)
}

// compactRedaction summarizes the thread through the end of the step
// holding its latest uncovered redaction, from a transcript that leaves
// the redacted events out (spec 010): an edited history would invalidate
// the provider's thinking signatures, and the removed value must not
// reach the model again.
func (t *turn) compactRedaction(ctx context.Context) error {
	pending, err := session.Uncompacted(t.events(), t.thread)
	if err != nil || len(pending) == 0 {
		return err
	}
	last := pending[len(pending)-1].Seq
	evs := t.events()
	to := evs[len(evs)-1].Seq
	for _, e := range t.steps() {
		if e.Seq > last {
			to = e.Seq - 1
			break
		}
	}
	done, err := t.summarize(ctx, 0, to, session.CauseRedaction)
	if err != nil {
		return err
	}
	if !done {
		return fmt.Errorf("%w: nothing to summarize before seq %d", session.ErrRedactionUncompacted, to)
	}
	return nil
}

// summarize asks the thread's own model for a summary of the thread's
// events through to, and appends the summary compaction with its cause.
// It reports false when the range holds none of the thread's events.
func (t *turn) summarize(ctx context.Context, before int64, to uint64, cause string) (bool, error) {
	var from uint64
	var prefix []session.Event
	for _, e := range t.events() {
		if e.Seq > to {
			break
		}
		prefix = append(prefix, e)
		if from == 0 && e.Thread == t.thread {
			from = e.Seq
		}
	}
	if from == 0 || from > to {
		return false, nil
	}
	tr, err := session.FoldOmittingRedacted(prefix, t.thread)
	if err != nil {
		return false, err
	}
	ask := lux.Block{Type: ir.BlockText, Text: prompts.Text(prompts.Compaction)}
	if n := len(tr.Messages); n > 0 && tr.Messages[n-1].Role == ir.RoleUser {
		tr.Messages[n-1].Blocks = append(tr.Messages[n-1].Blocks, ask)
	} else {
		tr.Messages = append(tr.Messages, lux.Message{Role: ir.RoleUser, Blocks: []lux.Block{ask}})
	}
	req, toolsSHA, err := t.request(ctx, tr)
	if err != nil {
		return false, err
	}
	req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceNone}
	began := t.h.c.Clock()
	res, attempts, err := t.send(ctx, req, false)
	if err != nil {
		se, eerr := t.sessionError(CodeCompactionFailed, err.Error(), models.Retryable(err), "")
		if eerr != nil {
			return false, eerr
		}
		return false, t.finish(ctx, session.StopError, CodeCompactionFailed, se)
	}
	si := sendInfo{maxTokens: *req.MaxTokens, toolsSHA: toolsSHA, attempts: attempts, latency: t.h.c.Clock().Sub(began)}
	mr, err := t.modelRequest(ctx, res, si, "ok")
	if err != nil {
		return false, err
	}
	var summary []string
	for _, b := range res.Message.Blocks {
		if b.Type == ir.BlockText && strings.TrimSpace(b.Text) != "" {
			summary = append(summary, strings.TrimSpace(b.Text))
		}
	}
	if len(summary) == 0 {
		se, err := t.sessionError(CodeCompactionFailed, "the compaction answered with no summary", true, "")
		if err != nil {
			return false, err
		}
		return false, t.finish(ctx, session.StopError, CodeCompactionFailed, mr, se)
	}
	c := session.ContextCompacted{Kind: session.CompactSummary, FromSeq: from, ToSeq: to, Summary: strings.Join(summary, "\n\n"), Cause: cause, Request: mr.ID, Prompt: string(prompts.Compaction), TokensBefore: before}
	e, err := t.event(session.TypeContextCompacted, c)
	if err != nil {
		return false, err
	}
	after, err := t.estimateWith(ctx, mr, e)
	if err != nil {
		return false, err
	}
	c.TokensAfter = after
	if e, err = t.event(session.TypeContextCompacted, c); err != nil {
		return false, err
	}
	return true, t.commit(ctx, mr, e)
}
