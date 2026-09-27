// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"time"
)

// Limits of spec 008's web_fetch, which every Fetcher enforces.
const (
	FetchTimeout   = 30 * time.Second
	FetchRedirects = 5
	FetchMaxBody   = 10 << 20
)

// Fetcher is a machine that can fetch a URL from inside its own network
// boundary. web_fetch reaches the network only through it, so a fetch
// on a Cella machine runs in the sandbox and never from the runner's
// network. A machine without network access does not implement it.
type Fetcher interface {
	// Fetch GETs r.URL, an http or https URL, following at most
	// FetchRedirects redirects and reading at most FetchMaxBody bytes of
	// the body. A fetch that passes FetchTimeout returns an error that
	// wraps context.DeadlineExceeded; a response of any status is a
	// result, not an error.
	Fetch(ctx context.Context, r FetchRequest) (FetchResult, error)
}

// FetchRequest is one fetch.
type FetchRequest struct {
	URL string
}

// FetchResult is a fetched response.
type FetchResult struct {
	// URL is the final URL, after redirects.
	URL         string
	Status      int
	ContentType string
	Body        []byte
	// Truncated is set when the body was longer than FetchMaxBody and
	// was cut there.
	Truncated bool
}
