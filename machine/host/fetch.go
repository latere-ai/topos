// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"latere.ai/x/pkg/otel"

	"latere.ai/x/topos/machine"
)

// fetchTimeout, fetchRedirects and fetchMaxBody are machine's fetch
// limits, variables so a test can shorten them.
var (
	fetchTimeout   = machine.FetchTimeout
	fetchRedirects = machine.FetchRedirects
	fetchMaxBody   = int64(machine.FetchMaxBody)
)

// fetchAccept prefers Markdown, which sites that negotiate for agents
// serve, then HTML and plain text.
const fetchAccept = "text/markdown, text/html;q=0.9, text/plain;q=0.8, */*;q=0.5"

// fetchUserAgent names the fetcher to the sites it reaches.
const fetchUserAgent = "topos-web-fetch/1"

// fetchClient is the client of every host fetch: traced, and refusing a
// redirect past the limit or to a scheme other than http and https.
var fetchClient = &http.Client{
	Transport: otel.Transport(nil),
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > fetchRedirects {
			return fmt.Errorf("machine: stopped after %d redirects", fetchRedirects)
		}
		if err := checkScheme(req.URL); err != nil {
			return err
		}
		return nil
	},
}

// checkScheme refuses a URL that is not http or https, or names no host.
func checkScheme(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("machine: %q is not an http or https URL", u.Redacted())
	}
	if u.Host == "" {
		return fmt.Errorf("machine: %q names no host", u.Redacted())
	}
	return nil
}

// Fetch GETs a URL from the host's own network, which the host sandbox's
// network rule governs (spec 012).
func (h *Host) Fetch(ctx context.Context, r machine.FetchRequest) (res machine.FetchResult, err error) {
	h.mu.Lock()
	released := h.released
	h.mu.Unlock()
	if released {
		return machine.FetchResult{}, machine.ErrReleased
	}
	u, err := url.Parse(r.URL)
	if err != nil {
		return machine.FetchResult{}, fmt.Errorf("machine: the URL: %w", err)
	}
	if err := checkScheme(u); err != nil {
		return machine.FetchResult{}, err
	}
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return machine.FetchResult{}, fmt.Errorf("machine: the request: %w", err)
	}
	req.Header.Set("Accept", fetchAccept)
	req.Header.Set("User-Agent", fetchUserAgent)
	resp, err := fetchClient.Do(req)
	if err != nil {
		return machine.FetchResult{}, fetchError(ctx, fctx, err)
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBody+1))
	if err != nil {
		return machine.FetchResult{}, fetchError(ctx, fctx, err)
	}
	res = machine.FetchResult{
		URL:         resp.Request.URL.String(),
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        body,
	}
	if int64(len(body)) > fetchMaxBody {
		res.Body, res.Truncated = body[:fetchMaxBody], true
	}
	return res, nil
}

// fetchError names a fetch that passed its timeout with
// context.DeadlineExceeded, as the Fetcher contract says, and wraps any
// other failure.
func fetchError(parent, fctx context.Context, err error) error {
	if parent.Err() == nil && errors.Is(fctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("machine: the fetch passed its %s timeout: %w", fetchTimeout, context.DeadlineExceeded)
	}
	return fmt.Errorf("machine: fetch: %w", err)
}

var _ machine.Fetcher = (*Host)(nil)
