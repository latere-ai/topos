// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package parser

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"latere.ai/x/topos/harness/parsing"
	"latere.ai/x/topos/models"
)

// Folder is the session's folder of a files store as a read through the
// service needs it.
type Folder interface {
	// Locate answers the ref of the file the folder keeps for path,
	// relative to the working directory, with bytes of the hex digest
	// sha256, and parsing.ErrNotKept when it keeps none.
	Locate(ctx context.Context, path, sha256 string) (string, error)
	// Presign answers an address that reads the file at ref with no
	// credential, for a few minutes.
	Presign(ctx context.Context, ref string) (string, error)
	// Origin is where the file at ref is, as the service records it.
	Origin(ref, sha256 string) Origin
}

// Pricer answers the figures a model's tokens are priced at,
// models.ErrUnpriced for a model no source prices.
type Pricer func(ctx context.Context, model string) (models.Entry, error)

// The schedule of a read: a parse and a document not ready are read
// again every pollFirst, doubling to pollMax; the service's deadline is
// the time left less grace, so it stops reading and spending first, and a
// wait gives up grace/2 before the read does, which leaves the time to
// price what it read.
const (
	pollFirst = time.Second
	pollMax   = 10 * time.Second
	grace     = 30 * time.Second
	// priceWorkers is how many pages' records are read at once to price
	// a parse.
	priceWorkers = 8
)

// Reader reads a PDF of one session's folder through the service; it is
// a parsing.Reader. It keeps each parse that succeeded by the digest of
// its bytes for its life, and a read of bytes another read is parsing
// waits for that parse.
type Reader struct {
	c      *Client
	folder Folder
	price  Pricer
	log    *slog.Logger

	// timeout, poll, pollMax and grace are the schedule's, which a test
	// shortens.
	timeout, poll, pollMax, grace time.Duration

	mu      sync.Mutex
	kept    map[string]kept
	flights map[string]*flight
	prices  map[string]models.Entry
}

// kept is a parse that succeeded.
type kept struct {
	id            string
	pages, failed int
}

// flight is a parse one read runs and others wait for.
type flight struct {
	done chan struct{}
	err  error
}

var _ parsing.Reader = (*Reader)(nil)

// NewReader is the reader over c and folder, pricing with price. A nil
// log discards.
func NewReader(c *Client, folder Folder, price Pricer, log *slog.Logger) (*Reader, error) {
	if c == nil || folder == nil || price == nil {
		return nil, errors.New("parser: no client, no folder or no pricer")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Reader{
		c: c, folder: folder, price: price, log: log,
		timeout: parsing.Timeout, poll: pollFirst, pollMax: pollMax, grace: grace,
		kept: map[string]kept{}, flights: map[string]*flight{}, prices: map[string]models.Entry{},
	}, nil
}

// Read answers the pages r names of the PDF r's path and digest name,
// within parsing.Timeout.
func (rd *Reader) Read(ctx context.Context, r parsing.Request) (parsing.Document, error) {
	ctx, cancel := context.WithTimeout(ctx, rd.timeout)
	defer cancel()
	expr := cmp.Or(r.Pages, parsing.DefaultSelection)
	if err := parsing.CheckPages(expr); err != nil {
		return parsing.Document{}, err
	}
	k, doc, err := rd.parse(ctx, r)
	if err != nil {
		return doc, err
	}
	doc.ID, doc.Pages, doc.More, doc.Failed = k.id, k.pages, k.pages >= parsing.MaxPages, k.failed
	shown, err := parsing.Select(expr, k.pages)
	if err != nil {
		return doc, err
	}
	if len(shown) == 0 {
		return doc, &parsing.PastEnd{Pages: k.pages, More: doc.More}
	}
	text, err := rd.document(ctx, k.id, parsing.Format(shown))
	if err != nil {
		return doc, err
	}
	doc.Shown, doc.Text = shown, text
	return doc, nil
}

// parse is the parse of r's bytes that succeeded: the one kept, the one
// another read is running once it ends, or one this read runs. Only the
// read that ran it answers its cost.
func (rd *Reader) parse(ctx context.Context, r parsing.Request) (kept, parsing.Document, error) {
	rd.mu.Lock()
	if k, ok := rd.kept[r.SHA256]; ok {
		rd.mu.Unlock()
		return k, parsing.Document{}, nil
	}
	if f, ok := rd.flights[r.SHA256]; ok {
		rd.mu.Unlock()
		select {
		case <-ctx.Done():
			return kept{}, parsing.Document{}, ctx.Err()
		case <-f.done:
		}
		if f.err != nil {
			return kept{}, parsing.Document{}, f.err
		}
		rd.mu.Lock()
		k := rd.kept[r.SHA256]
		rd.mu.Unlock()
		return k, parsing.Document{}, nil
	}
	f := &flight{done: make(chan struct{})}
	rd.flights[r.SHA256] = f
	rd.mu.Unlock()

	k, doc, err := rd.run(ctx, r)
	rd.mu.Lock()
	delete(rd.flights, r.SHA256)
	if err == nil {
		rd.kept[r.SHA256] = k
	}
	f.err = err
	close(f.done)
	rd.mu.Unlock()
	return k, doc, err
}

// run reads again the parse r knows of, when it succeeded, or submits
// the file and waits for the parse to end.
func (rd *Reader) run(ctx context.Context, r parsing.Request) (kept, parsing.Document, error) {
	if r.Known != "" {
		p, err := rd.c.Get(ctx, r.Known)
		ref, refused := errors.AsType[*parsing.Refused](err)
		switch {
		case err == nil && p.State == StateSucceeded:
			return keptOf(p), parsing.Document{}, nil
		case err == nil, refused && (ref.Status == http.StatusNotFound || ref.Status == http.StatusForbidden):
			// A parse past its retention, another owner's, or one that did
			// not succeed: the file is parsed again, and the service reuses
			// every page it already read.
		default:
			return kept{}, parsing.Document{}, err
		}
	}
	ref, err := rd.folder.Locate(ctx, r.Path, r.SHA256)
	if err != nil {
		return kept{}, parsing.Document{}, err
	}
	until, deadline, err := rd.budget(ctx)
	if err != nil {
		return kept{}, parsing.Document{}, err
	}
	u, err := rd.folder.Presign(ctx, ref)
	if err != nil {
		return kept{}, parsing.Document{}, fmt.Errorf("%w: presign %s: %w", parsing.ErrUnavailable, ref, err)
	}
	p, err := rd.c.Submit(ctx, Submit{
		URL: u, Origin: rd.folder.Origin(ref, r.SHA256), Pages: fmt.Sprintf("1-%d", parsing.MaxPages),
		AllowFailedPages: parsing.MaxPages, Deadline: deadline,
	}, min(MaxWait, deadline))
	if err != nil {
		return kept{}, parsing.Document{}, err
	}
	p, werr := rd.wait(ctx, p, until)
	if ctx.Err() != nil && werr != nil {
		return kept{}, parsing.Document{}, werr
	}
	var doc parsing.Document
	doc.CostUSDMicro, doc.Unpriced = rd.cost(ctx, p)
	switch {
	case werr != nil:
		return kept{}, doc, werr
	case p.State != StateSucceeded:
		f := &parsing.Failed{State: p.State}
		if p.Error != nil {
			f.Code, f.Detail = p.Error.Code, p.Error.Detail
		}
		return kept{}, doc, f
	}
	return keptOf(p), doc, nil
}

// budget is when a wait gives up and the deadline the service is given,
// from the time the read has left.
func (rd *Reader) budget(ctx context.Context) (time.Time, time.Duration, error) {
	end, ok := ctx.Deadline()
	if !ok {
		return time.Time{}, 0, errors.New("parser: a read without a deadline")
	}
	deadline := time.Until(end) - rd.grace
	if deadline < time.Second {
		return time.Time{}, 0, fmt.Errorf("parser: %w before the file was submitted", context.DeadlineExceeded)
	}
	return end.Add(-rd.grace / 2), deadline.Truncate(time.Second), nil
}

// keptOf is the record of a parse that succeeded.
func keptOf(p Parse) kept {
	return kept{id: p.ID, pages: p.Progress.PagesTotal, failed: p.Progress.PagesFailed}
}

// wait reads p again on the poll's schedule until it ends or until; a
// parse still running then is a timeout.
func (rd *Reader) wait(ctx context.Context, p Parse, until time.Time) (Parse, error) {
	every := rd.poll
	for !p.Ended() {
		left := time.Until(until)
		if left <= 0 {
			return p, fmt.Errorf("parser: the parse %s is still %s: %w", p.ID, p.State, context.DeadlineExceeded)
		}
		if err := sleep(ctx, min(every, left)); err != nil {
			return p, err
		}
		every = min(every*2, rd.pollMax)
		next, err := rd.c.Get(ctx, p.ID)
		if err != nil {
			return p, err
		}
		p = next
	}
	return p, nil
}

// document reads the Markdown of pages, asked again on the poll's
// schedule while the service has not assembled it.
func (rd *Reader) document(ctx context.Context, id, pages string) (string, error) {
	every := rd.poll
	for {
		text, err := rd.c.Document(ctx, id, pages)
		ref, refused := errors.AsType[*parsing.Refused](err)
		if !refused || ref.Status != http.StatusConflict || ref.Code != CodeNotReady {
			return text, err
		}
		if err := sleep(ctx, every); err != nil {
			return "", err
		}
		every = min(every*2, rd.pollMax)
	}
}

// sleep waits d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// cost prices the model reads of p: zero for a parse whose usage names no
// tokens, its pages read from their text layer or reused, and otherwise
// each page a model read, not reused, at its model's figures, the tokens
// of one model summed before they are priced. A cost that could not be
// had, a model unpriced or a page that could not be read, is nil and
// unpriced, and logged.
func (rd *Reader) cost(ctx context.Context, p Parse) (*int64, bool) {
	if p.Usage.InputTokens == 0 && p.Usage.OutputTokens == 0 {
		zero := int64(0)
		return &zero, false
	}
	unpriced := func(err error) (*int64, bool) {
		rd.log.ErrorContext(ctx, "price a parse's model reads", "parse", p.ID, "input_tokens", p.Usage.InputTokens, "output_tokens", p.Usage.OutputTokens, "err", err)
		return nil, true
	}
	list, err := rd.c.Pages(ctx, p.ID)
	if err != nil {
		return unpriced(err)
	}
	var read []int
	for _, s := range list {
		if s.Source == SourceReader {
			read = append(read, s.Number)
		}
	}
	byModel, err := rd.pageUsage(ctx, p.ID, read)
	if err != nil {
		return unpriced(err)
	}
	names := make([]string, 0, len(byModel))
	for m := range byModel {
		names = append(names, m)
	}
	slices.Sort(names)
	var total int64
	for _, m := range names {
		e, err := rd.figures(ctx, m)
		if err != nil {
			return unpriced(fmt.Errorf("the model %q: %w", m, err))
		}
		u := byModel[m]
		c, _, err := models.Cost(models.UsageFigures{Input: u.InputTokens, Output: u.OutputTokens}, e)
		if err != nil {
			return unpriced(fmt.Errorf("the model %q: %w", m, err))
		}
		total += c
	}
	return &total, false
}

// pageUsage sums the tokens of each of pages a model read in this parse,
// by model, reading priceWorkers pages' records at once.
func (rd *Reader) pageUsage(ctx context.Context, id string, pages []int) (map[string]Usage, error) {
	var (
		mu    sync.Mutex
		out   = map[string]Usage{}
		errs  []error
		wg    sync.WaitGroup
		queue = make(chan int)
	)
	for range min(priceWorkers, len(pages)) {
		wg.Go(func() {
			for n := range queue {
				pg, err := rd.c.Page(ctx, id, n)
				mu.Lock()
				switch {
				case err != nil:
					errs = append(errs, fmt.Errorf("page %d: %w", n, err))
				case pg.Reused || pg.Usage == nil || pg.Usage.InputTokens+pg.Usage.OutputTokens == 0:
				default:
					u := out[pg.Model]
					u.InputTokens += pg.Usage.InputTokens
					u.OutputTokens += pg.Usage.OutputTokens
					out[pg.Model] = u
				}
				mu.Unlock()
			}
		})
	}
	for _, n := range pages {
		queue <- n
	}
	close(queue)
	wg.Wait()
	return out, errors.Join(errs...)
}

// figures are a model's figures, asked of the pricer once per model.
func (rd *Reader) figures(ctx context.Context, model string) (models.Entry, error) {
	rd.mu.Lock()
	e, ok := rd.prices[model]
	rd.mu.Unlock()
	if ok {
		return e, nil
	}
	e, err := rd.price(ctx, model)
	if err != nil {
		return models.Entry{}, err
	}
	rd.mu.Lock()
	rd.prices[model] = e
	rd.mu.Unlock()
	return e, nil
}
