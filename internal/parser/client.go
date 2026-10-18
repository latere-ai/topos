// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package parser is toposd's client of the parsing service read sends a
// PDF to (spec 080), and the Reader the hosted builder hands the tool:
// it finds the file in the session's folder, has the files API presign
// it, submits it with its origin, waits for the parse, prices its model
// reads and answers the pages a call asks for as Markdown. Every request
// carries the bearer its token function answers at that request.
package parser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/topos/harness/parsing"
)

// The bounds of an answer's body: a parse or a list of pages, one page
// with its blocks, and an error envelope. A document's is parsing.MaxText.
const (
	maxParseBody = 1 << 20
	maxPageBody  = 8 << 20
	maxErrorBody = 64 << 10
)

// errPastLimit is an answer whose body passed its bound.
var errPastLimit = errors.New("parser: the answer passed its bound")

// MaxWait is the most seconds a submit asks the service to hold its
// answer for with Prefer: wait, the most it holds one.
const MaxWait = 60 * time.Second

// Parse states the service answers.
const (
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateCanceled  = "canceled"
)

// SourceReader is a page a model read from its image, the one source a
// parse is charged for.
const SourceReader = "reader"

// CodeNotReady is the service's refusal of a document it has not
// assembled yet, which is asked again.
const CodeNotReady = "document_not_ready"

// Token answers the bearer of one request.
type Token func(ctx context.Context) (string, error)

// Client is the parsing service at one base URL.
type Client struct {
	base  string
	token Token
	http  *http.Client
}

// New builds the client of the service at base, an absolute http or
// https URL under which parses/ is served.
func New(base string, token Token, c *http.Client) (*Client, error) {
	u, err := url.Parse(base)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return nil, fmt.Errorf("parser: %q is not an absolute http or https URL", base)
	case token == nil || c == nil:
		return nil, errors.New("parser: no token or no HTTP client")
	}
	return &Client{base: strings.TrimRight(base, "/"), token: token, http: c}, nil
}

// Origin is where a parse's file is kept, which the service records and
// an authorizer reads: the files service's name, the file's path in it,
// and the version of its bytes.
type Origin struct {
	Store   string `json:"store"`
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
}

// Submit is one parse asked of the service.
type Submit struct {
	URL              string
	Origin           Origin
	Pages            string
	AllowFailedPages int
	// Deadline is when the service stops reading, from its receipt; zero
	// names none.
	Deadline time.Duration
}

// Usage is the tokens a parse or a page's model read.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// Parse is a parse as the service answers it.
type Parse struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Progress struct {
		PagesTotal  int `json:"pages_total"`
		PagesDone   int `json:"pages_done"`
		PagesFailed int `json:"pages_failed"`
	} `json:"progress"`
	Usage Usage `json:"usage"`
	Error *struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	} `json:"error"`
}

// Ended reports a parse that will not change: succeeded, failed or
// canceled.
func (p Parse) Ended() bool {
	return p.State == StateSucceeded || p.State == StateFailed || p.State == StateCanceled
}

// PageSummary is one page of a parse's list.
type PageSummary struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Source string `json:"source"`
}

// Page is one page's record of what read it: its source, its reader's
// model, whether an earlier read was reused, and its tokens.
type Page struct {
	Number int    `json:"number"`
	Source string `json:"source"`
	Model  string `json:"model"`
	Reused bool   `json:"reused"`
	Usage  *Usage `json:"usage"`
}

// Submit asks for a parse, the service holding its answer up to wait,
// at most MaxWait, for a parse that ends within it.
func (c *Client) Submit(ctx context.Context, s Submit, wait time.Duration) (Parse, error) {
	body := map[string]any{
		"source": map[string]string{"url": s.URL},
		"origin": s.Origin,
		"pages":  s.Pages,
	}
	if s.AllowFailedPages > 0 {
		body["allow_failed_pages"] = s.AllowFailedPages
	}
	if s.Deadline > 0 {
		body["deadline"] = strconv.Itoa(int(s.Deadline/time.Second)) + "s"
	}
	h := http.Header{}
	if secs := int(min(wait, MaxWait) / time.Second); secs > 0 {
		h.Set("Prefer", "wait="+strconv.Itoa(secs))
	}
	var p Parse
	return p, c.json(ctx, http.MethodPost, "/parses", body, h, &p)
}

// Get reads a parse.
func (c *Client) Get(ctx context.Context, id string) (Parse, error) {
	var p Parse
	return p, c.json(ctx, http.MethodGet, "/parses/"+url.PathEscape(id), nil, nil, &p)
}

// Pages lists a parse's pages.
func (c *Client) Pages(ctx context.Context, id string) ([]PageSummary, error) {
	var out struct {
		Pages []PageSummary `json:"pages"`
	}
	return out.Pages, c.json(ctx, http.MethodGet, "/parses/"+url.PathEscape(id)+"/pages", nil, nil, &out)
}

// Page reads one page's record.
func (c *Client) Page(ctx context.Context, id string, n int) (Page, error) {
	var p Page
	return p, c.json(ctx, http.MethodGet, "/parses/"+url.PathEscape(id)+"/pages/"+strconv.Itoa(n), nil, nil, &p)
}

// Document reads the Markdown of a parse's pages, each opened by the
// service's page mark. A text past parsing.MaxText is ErrTooMuchText.
func (c *Client) Document(ctx context.Context, id, pages string) (string, error) {
	q := url.Values{"format": {"markdown"}, "page_breaks": {"true"}, "pages": {pages}}
	raw, err := c.do(ctx, http.MethodGet, "/parses/"+url.PathEscape(id)+"/document?"+q.Encode(), nil, nil, parsing.MaxText)
	switch {
	case errors.Is(err, errPastLimit):
		return "", parsing.ErrTooMuchText
	case err != nil:
		return "", err
	}
	return string(raw), nil
}

// json sends one request and decodes a 2xx's body into out.
func (c *Client) json(ctx context.Context, method, path string, in any, h http.Header, out any) error {
	limit := int64(maxParseBody)
	if strings.Contains(path, "/pages/") {
		limit = maxPageBody
	}
	raw, err := c.do(ctx, method, path, in, h, limit)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: the answer to %s %s does not decode: %w", parsing.ErrUnavailable, method, path, err)
	}
	return nil
}

// do sends one request with the bearer and answers a 2xx's body of at
// most limit bytes; a refusal with the envelope is a parsing.Refused, and
// every other failure parsing.ErrUnavailable.
func (c *Client) do(ctx context.Context, method, path string, in any, h http.Header, limit int64) ([]byte, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("parser: encode the request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, fmt.Errorf("parser: build the request: %w", err)
	}
	maps.Copy(req.Header, h)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	tok, err := c.token(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: the credential: %w", parsing.ErrUnavailable, err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s %s: %w", parsing.ErrUnavailable, method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		if err != nil {
			return nil, fmt.Errorf("%w: %s %s answered %d, and its body could not be read: %w", parsing.ErrUnavailable, method, path, resp.StatusCode, err)
		}
		if ref := refusal(resp.StatusCode, raw); ref != nil && resp.StatusCode < 500 {
			return nil, ref
		}
		return nil, fmt.Errorf("%w: %s %s answered %d", parsing.ErrUnavailable, method, path, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read the answer to %s %s: %w", parsing.ErrUnavailable, method, path, err)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%w: %w: the answer to %s %s passed %d bytes", parsing.ErrUnavailable, errPastLimit, method, path, limit)
	}
	return raw, nil
}

// refusal reads an error envelope, nil when the body is none.
func refusal(status int, raw []byte) *parsing.Refused {
	var env struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Error == nil || env.Error.Code == "" {
		return nil
	}
	return &parsing.Refused{Status: status, Code: env.Error.Code, Message: strings.TrimSpace(env.Error.Message)}
}
