// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// Rebuild is the harness's half of models.Replay (spec 007) for a
// session this harness's configuration ran: each recorded request built
// again as its step built it, the fold of the log through the request's
// fold_seq, on the thread it ran on, with the prompt version, the model
// and the dialect it recorded. A thread's configuration is its parent's
// narrowed by its thread.started, as spawn and the advisor built it, and
// a compaction's summary request is built from the range it summarized.
// blobs reads the instruction files the fold names.
//
// A step whose log was redacted after it was sent, and one on a thread
// whose subagent the configuration no longer names, cannot be built
// again and is skipped.
func (h *Harness) Rebuild(s session.Session, blobs BlobReader) models.Rebuild {
	return func(ctx context.Context, log []session.Event, step session.Event, mr session.ModelRequest) (models.Request, error) {
		if mr.FoldSeq == 0 {
			return models.Request{}, fmt.Errorf("%w: the request recorded no fold_seq", models.ErrNotRebuilt)
		}
		var prefix []session.Event
		for _, e := range log {
			if e.Seq <= mr.FoldSeq {
				prefix = append(prefix, e)
			}
		}
		root := &turn{h: h, s: s, sh: &shared{events: prefix}, l: replayLog{blobs}, root: h.c.Tools}
		var err error
		if root.reg, err = root.registry(h.c.Tools.Names()); err != nil {
			return models.Request{}, err
		}
		t, err := root.replayThread(log, step.Thread)
		if err != nil {
			return models.Request{}, err
		}
		cfg := t.h.c
		if cfg.PromptVersion, err = promptVersion(mr.PromptVersion); err != nil {
			return models.Request{}, err
		}
		cfg.Connection.Model, cfg.Connection.Family, cfg.Connection.Dialect = mr.Model, mr.Family, ir.Dialect(mr.Dialect)
		if t.h, err = New(cfg); err != nil {
			return models.Request{}, err
		}
		req, err := t.replayRequest(ctx, log, prefix, step)
		if err != nil {
			return models.Request{}, err
		}
		return models.Request{IR: req, Connection: cfg.Connection}, nil
	}
}

// replayRequest builds a step's request from the log through its fold
// point, or, for a compaction's summary request, from the range the
// compaction summarized with the summary's ask.
func (t *turn) replayRequest(ctx context.Context, log, prefix []session.Event, step session.Event) (ir.Request, error) {
	if c, ok := summaryOf(log, step.ID); ok {
		var summarized []session.Event
		for _, e := range log {
			if e.Seq <= c.ToSeq {
				summarized = append(summarized, e)
			}
		}
		tr, err := session.FoldOmittingRedacted(summarized, t.thread)
		if err != nil {
			return ir.Request{}, err
		}
		ask := lux.Block{Type: ir.BlockText, Text: prompts.Text(prompts.Compaction)}
		if n := len(tr.Messages); n > 0 && tr.Messages[n-1].Role == ir.RoleUser {
			tr.Messages[n-1].Blocks = append(tr.Messages[n-1].Blocks, ask)
		} else {
			tr.Messages = append(tr.Messages, lux.Message{Role: ir.RoleUser, Blocks: []lux.Block{ask}})
		}
		req, _, err := t.request(ctx, tr)
		req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceNone}
		return req, err
	}
	tr, err := session.Fold(prefix, t.thread)
	if errors.Is(err, session.ErrRedactionUncompacted) {
		return ir.Request{}, fmt.Errorf("%w: the log was redacted after the request: %w", models.ErrNotRebuilt, err)
	}
	if err != nil {
		return ir.Request{}, err
	}
	req, _, err := t.request(ctx, tr)
	return req, err
}

// replayThread is the turn of thread id under t, built as spawn and the
// advisor built it: its parent's, then the configuration and the tools
// its thread.started names.
func (t *turn) replayThread(log []session.Event, id string) (*turn, error) {
	if id == t.thread {
		return t, nil
	}
	started, ok := threadStartedOf(log, id)
	if !ok {
		return nil, fmt.Errorf("%w: the log has no thread.started for thread %s", models.ErrNotRebuilt, id)
	}
	parent, err := t.replayThread(log, started.Parent)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if sub, ok := parent.h.c.Subagents[started.Agent.Name]; ok {
		cfg = parent.childConfig(sub)
	} else if started.Agent.Name == advisorAgent && parent.h.c.Advisor != nil {
		cfg = parent.advisorConfig()
	} else {
		return nil, fmt.Errorf("%w: the configuration names no subagent %q for thread %s", models.ErrNotRebuilt, started.Agent.Name, id)
	}
	return parent.child(id, started.Depth, cfg, started.Tools)
}

func threadStartedOf(log []session.Event, id string) (session.ThreadStarted, bool) {
	for _, e := range log {
		if e.Type != session.TypeThreadStarted || e.ID != id || e.Redacted() {
			continue
		}
		var p session.ThreadStarted
		if e.Decode(&p) == nil {
			return p, true
		}
	}
	return session.ThreadStarted{}, false
}

// summaryOf finds the summary compaction whose request is the
// model.request id.
func summaryOf(log []session.Event, id string) (session.ContextCompacted, bool) {
	for _, e := range log {
		if e.Type != session.TypeContextCompacted || e.Redacted() {
			continue
		}
		var c session.ContextCompacted
		if e.Decode(&c) == nil && c.Kind == session.CompactSummary && c.Request == id {
			return c, true
		}
	}
	return session.ContextCompacted{}, false
}

// promptVersion reads a recorded prompt_version, "harness/<n>".
func promptVersion(v string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(v, "harness/"))
	if err != nil || !strings.HasPrefix(v, "harness/") {
		return 0, fmt.Errorf("%w: the prompt version %q is not harness/<n>", models.ErrNotRebuilt, v)
	}
	return n, nil
}

// errReplayWrite is a replay's log asked to append or store: a replay
// builds requests and writes nothing.
var errReplayWrite = errors.New("harness: a replay writes nothing to the log")

// replayLog is the log a replay's turns read instruction blobs from.
type replayLog struct{ BlobReader }

func (replayLog) Append(context.Context, []session.Event) ([]session.Event, error) {
	return nil, errReplayWrite
}

func (replayLog) PutBlob(context.Context, io.Reader) (session.Digest, error) {
	return "", errReplayWrite
}
