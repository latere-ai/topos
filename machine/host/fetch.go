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
	"strings"

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

// egressAllows reports whether a host name is one of egress: the name
// itself, or a subdomain of a "*." entry's domain.
func egressAllows(egress []string, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, e := range egress {
		if domain, ok := strings.CutPrefix(e, "*."); ok {
			if strings.HasSuffix(host, "."+domain) {
				return true
			}
			continue
		}
		if host == e {
			return true
		}
	}
	return false
}

// checkEgress refuses a URL whose host a sandboxed machine may not
// reach. A machine without a sandbox reaches every host.
func (h *Host) checkEgress(u *url.URL) error {
	if h.opts.Sandbox == nil || egressAllows(h.opts.Sandbox.Egress, u.Hostname()) {
		return nil
	}
	return fmt.Errorf("machine: %s is not one of the hosts this machine may reach", u.Hostname())
}

// Fetch GETs a URL from the host's own network, which the host sandbox's
// network rule governs (spec 012). A sandboxed machine's fetch, the URL
// and every redirect, reaches only the sandbox's egress hosts, the
// allowlist its commands are held to.
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
	if err := h.checkEgress(u); err != nil {
		return machine.FetchResult{}, err
	}
	client := fetchClient
	if h.opts.Sandbox != nil {
		c := *fetchClient
		c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if err := fetchClient.CheckRedirect(req, via); err != nil {
				return err
			}
			return h.checkEgress(req.URL)
		}
		client = &c
	}
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return machine.FetchResult{}, fmt.Errorf("machine: the request: %w", err)
	}
	req.Header.Set("Accept", fetchAccept)
	req.Header.Set("User-Agent", fetchUserAgent)
	resp, err := client.Do(req)
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
