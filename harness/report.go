// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"
	"latere.ai/x/pkg/llmdialect/tokencount"

	"latere.ai/x/topos/session"
)

// requestShares are the estimates of a sent request's parts (spec 087),
// with the estimate of the whole request the turn's bias corrects.
type requestShares struct {
	instructions, tools, messages, files, summary int64
	estimate                                      int64
}

// messageShares are the estimates of the parts of a transcript's messages
// the context report counts apart: the files persons brought and the
// summaries of compacted history.
type messageShares struct{ files, summary int64 }

// estimateOf is the counter's estimate of req, 0 for a request with no
// system prompt, messages or tools, which the counter would call 1.
func estimateOf(req ir.Request) int64 {
	if len(req.System) == 0 && len(req.Messages) == 0 && len(req.Tools) == 0 {
		return 0
	}
	return tokencount.Estimate(&req)
}

// estimateBlocks is the counter's estimate of the text of blocks, read
// as a system prompt, which carries no framing of its own.
func estimateBlocks(blocks []lux.Block) int64 {
	var sys []ir.Block
	for _, b := range blocks {
		if b.Type == ir.BlockText && b.Text != "" {
			sys = append(sys, ir.Block{Type: ir.BlockText, Text: b.Text})
		}
	}
	return estimateOf(ir.Request{System: sys})
}

// sharesOf are the estimates of tr's files and summaries.
func sharesOf(tr session.Transcript) messageShares {
	return messageShares{files: estimateBlocks(tr.Attached), summary: estimateBlocks(tr.Summaries)}
}

// shares are the estimates of req's parts, its messages' files and
// summary from the transcript the turn last built a request from.
func (t *turn) shares(req ir.Request) requestShares {
	s := requestShares{
		instructions: estimateOf(ir.Request{System: req.System}),
		tools:        estimateOf(ir.Request{Tools: req.Tools}),
		files:        t.messageShares.files,
		summary:      t.messageShares.summary,
		estimate:     t.estimate(req),
	}
	s.messages = max(estimateOf(ir.Request{Messages: req.Messages})-s.files-s.summary, 0)
	return s
}

// contextReport is the context a request records (spec 087): used is the
// provider's prompt tokens when it counted them, and the harness's
// estimate when it did not, as a failed or cut request; each part is its
// estimate scaled so the parts but free sum to used, the rounding's
// remainder on the largest, and free the window less used.
func (t *turn) contextReport(s requestShares, prompt int64) *session.ContextReport {
	used, source := prompt, session.ContextFromProvider
	if prompt <= 0 {
		used, source = s.estimate, session.ContextFromEstimate
	}
	window := t.h.c.Entry.InputWindow
	names := []string{session.ReportInstructions, session.ReportTools, session.ReportMessages, session.ReportFiles, session.ReportSummary}
	est := []int64{s.instructions, s.tools, s.messages, s.files, s.summary}
	var total int64
	for _, n := range est {
		total += n
	}
	scaled := make([]int64, len(est))
	if total > 0 {
		var sum int64
		largest := 0
		for i, n := range est {
			scaled[i] = n * used / total
			sum += scaled[i]
			if n > est[largest] {
				largest = i
			}
		}
		scaled[largest] += used - sum
	}
	r := &session.ContextReport{Window: window, Used: used, Source: source}
	for i, n := range names {
		r.Parts = append(r.Parts, session.ReportPart{Part: n, Tokens: scaled[i]})
	}
	r.Parts = append(r.Parts, session.ReportPart{Part: session.ReportFree, Tokens: max(window-used, 0)})
	return r
}
