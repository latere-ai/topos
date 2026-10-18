// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"path"
	"strings"

	"latere.ai/x/topos/harness/parsing"
	"latere.ai/x/topos/prompts"
)

// The reasons a PDF was not read as text, which results/read/pdf-unread
// words for the model (spec 082).
const (
	unreadKept        = "kept"
	unreadLarge       = "large"
	unreadTimeout     = "timeout"
	unreadRefused     = "refused"
	unreadFailed      = "failed"
	unreadUnavailable = "unavailable"
)

// codeDeadline is the code of a parse that ended at its deadline, which
// the model is told to wait for as it is a read past its time.
const codeDeadline = "deadline_exceeded"

// ParseMeta is the parse of a PDF a read read through a parsing service
// (spec 082): the service's id of it, how many pages it read, whether
// the document has more than those, the pages the result shows, and
// whether what the read cost could not be priced. A later drive of the
// thread reads the parse again by its id.
type ParseMeta struct {
	ID       string `json:"id"`
	Pages    int    `json:"pages"`
	More     bool   `json:"more,omitempty"`
	Shown    string `json:"shown"`
	Unpriced bool   `json:"unpriced,omitempty"`
}

// readPDF reads the PDF at p, its head already read through h, as the
// Markdown of the pages in's selection names, through pr. The whole file
// is hashed, at most ReadMaxPDF bytes, so the parse is of the bytes the
// machine holds; a file the service did not read is the binary answer
// and the sentence that says why, carrying what the read cost.
func readPDF(ctx context.Context, b *builtin, c Call, p string, in readInput, pr parsing.Reader, br io.Reader, h hash.Hash) (Result, error) {
	if in.Pages != "" {
		if err := parsing.CheckPages(in.Pages); err != nil {
			return Text(OutcomeInvalidInput, invalidInput(NameRead, "/pages: "+strings.TrimPrefix(err.Error(), "parsing: "))), nil
		}
	}
	// br still holds the head it peeked, so the copy counts the whole file.
	n, err := io.Copy(io.Discard, io.LimitReader(br, ReadMaxPDF+1))
	if err != nil {
		return b.fail(ctx, c, p, err)
	}
	if n > ReadMaxPDF {
		return unreadPDF(ctx, b, c, p, unreadLarge, nil, nil)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	rel, ok := inWorkdir(c, p)
	if !ok {
		return unreadPDF(ctx, b, c, p, unreadKept, nil, nil)
	}
	r := parsing.Request{Path: rel, SHA256: sum, Pages: in.Pages}
	if k, ok := c.State.Parses[sum]; ok {
		r.Known = k.ID
	}
	doc, err := pr.Read(ctx, r)
	if cerr := ctx.Err(); cerr != nil {
		return b.fail(ctx, c, p, cerr)
	}
	sel := in.Pages
	if sel == "" {
		sel = parsing.DefaultSelection
	}
	pastEnd, isPastEnd := errors.AsType[*parsing.PastEnd](err)
	switch {
	case isPastEnd:
		res, rerr := b.result(ctx, c, OutcomeError, prompts.Render(prompts.ReadPagesPastEnd, prompts.Data{"Path": p, "Count": plural(pastEnd.Pages, "page"), "More": pastEnd.More, "Selection": sel}), nil)
		res.CostUSDMicro = doc.CostUSDMicro
		return res, rerr
	case errors.Is(err, parsing.ErrTooMuchText):
		res, rerr := b.result(ctx, c, OutcomeError, prompts.Render(prompts.ReadPagesTooLarge, prompts.Data{"Path": p, "Selection": sel, "Max": parsing.MaxText >> 20}), nil)
		res.CostUSDMicro = doc.CostUSDMicro
		return res, rerr
	case err != nil:
		return unreadPDF(ctx, b, c, p, "", err, doc.CostUSDMicro)
	}
	shown := parsing.Format(doc.Shown)
	head := prompts.Render(prompts.ReadPDF, prompts.Data{"Path": p, "Count": plural(doc.Pages, "page"), "More": doc.More, "Failed": doc.Failed, "Shown": shown, "One": len(doc.Shown) == 1}) + "\n"
	tail := ""
	if next := parsing.Next(doc.Shown, doc.Pages); next != "" {
		tail = prompts.Render(prompts.ReadMorePages, prompts.Data{"Shown": shown, "Pages": doc.Pages, "Next": next}) + "\n"
	}
	res, err := readLines(ctx, b, c, p, in, bufio.NewReader(strings.NewReader(doc.Text)), h, head, tail)
	if err != nil {
		return Result{}, err
	}
	if res.Meta != nil {
		res.Meta.Parse = &ParseMeta{ID: doc.ID, Pages: doc.Pages, More: doc.More, Shown: shown, Unpriced: doc.Unpriced}
	}
	res.CostUSDMicro = doc.CostUSDMicro
	return res, nil
}

// inWorkdir is p relative to the machine's working directory, false for a
// path outside it, which no session's folder keeps.
func inWorkdir(c Call, p string) (string, bool) {
	wd := c.Machine.Info().Workdir
	if wd == "" {
		return "", false
	}
	rel, ok := strings.CutPrefix(p, path.Clean(wd)+"/")
	return rel, ok && rel != ""
}

// unreadPDF is the binary answer of the PDF at p and the sentence that
// says why the service did not read it: why, or the reason err names,
// with what the read cost.
func unreadPDF(ctx context.Context, b *builtin, c Call, p, why string, err error, cost *int64) (Result, error) {
	data := prompts.Data{"Why": why, "Max": ReadMaxPDF >> 20, "Timeout": parsing.Timeout.String(), "Message": "", "Code": ""}
	if why == "" {
		data["Why"] = unreadUnavailable
		ref, refused := errors.AsType[*parsing.Refused](err)
		failed, ended := errors.AsType[*parsing.Failed](err)
		switch {
		case errors.Is(err, parsing.ErrNotKept):
			data["Why"] = unreadKept
		case refused:
			// The sentence ends the model's, which closes it.
			data["Why"], data["Message"] = unreadRefused, strings.TrimSuffix(strings.TrimSpace(ref.Message), ".")
		case ended && failed.Code == codeDeadline, errors.Is(err, context.DeadlineExceeded):
			data["Why"] = unreadTimeout
		case ended:
			data["Why"], data["Code"] = unreadFailed, failed.Code
		}
	}
	text := prompts.Render(prompts.ReadBinary, prompts.Data{"Path": p}) + " " + prompts.Render(prompts.ReadPDFUnread, data)
	res, rerr := b.result(ctx, c, OutcomeError, text, nil)
	if rerr != nil {
		return Result{}, rerr
	}
	res.CostUSDMicro = cost
	return res, nil
}
