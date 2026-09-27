// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/topos/machine"
)

// fetchTimeout, fetchRedirects and fetchMaxBody are machine's fetch
// limits, variables so a test can shorten them.
var (
	fetchTimeout   = machine.FetchTimeout
	fetchRedirects = machine.FetchRedirects
	fetchMaxBody   = int64(machine.FetchMaxBody)
)

// fetchAccept and fetchUserAgent are the host machine's: Markdown first,
// which sites that negotiate for agents serve, then HTML and plain text.
const (
	fetchAccept    = "text/markdown, text/html;q=0.9, text/plain;q=0.8, */*;q=0.5"
	fetchUserAgent = "topos-web-fetch/1"
)

// fetchScript fetches $TOPOS_FETCH_URL with curl inside the sandbox, so
// Cella's egress gateway decides what it reaches. The body goes through
// head, which keeps one byte past the limit so a longer body reads as
// truncated. The answer is "ok" and three lines, the status, the content
// type and the final URL, then the body; or "error", curl's exit code and
// its message.
const fetchScript = `mkdir -p "$TOPOS_FETCH_DIR" || exit 1
d=$(mktemp -d "$TOPOS_FETCH_DIR/fetch-XXXXXX") || exit 1
trap 'rm -rf "$d"' EXIT
if ! command -v curl >/dev/null 2>&1; then
  echo "error 127 curl is not installed in the sandbox"
  exit 0
fi
{
  curl -sS -L --max-redirs "$TOPOS_FETCH_REDIRECTS" --proto =http,https --proto-redir =http,https \
    --max-time "$TOPOS_FETCH_SECONDS" -H "Accept: $TOPOS_FETCH_ACCEPT" -A "$TOPOS_FETCH_AGENT" \
    -w '%{http_code}\n%{content_type}\n%{url_effective}\n' -o /dev/fd/3 -- "$TOPOS_FETCH_URL" >"$d/meta" 2>"$d/err"
  echo $? >"$d/code"
} 3>&1 | head -c "$TOPOS_FETCH_LIMIT" >"$d/body"
code=$(cat "$d/code")
size=$(wc -c <"$d/body" | tr -d ' ')
if [ "$code" = 23 ] && [ "$size" -ge "$TOPOS_FETCH_LIMIT" ]; then code=0; fi
if [ "$code" != 0 ]; then
  printf 'error %s ' "$code"
  head -c 2000 "$d/err"
  exit 0
fi
echo ok
cat "$d/meta" "$d/body"`

// curlTimedOut is curl's exit code for an operation past --max-time.
const curlTimedOut = "28"

// Fetch GETs a URL from inside the sandbox, whose egress Cella's gateway
// decides, and never from the runner's network. It holds to the limits of
// machine.Fetcher: http and https only, at most five redirects, 10 MiB,
// 30 seconds.
func (m *Machine) Fetch(ctx context.Context, r machine.FetchRequest) (machine.FetchResult, error) {
	u, err := url.Parse(r.URL)
	if err != nil {
		return machine.FetchResult{}, fmt.Errorf("machine: the URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return machine.FetchResult{}, fmt.Errorf("machine: %q is not an http or https URL", u.Redacted())
	}
	if u.Host == "" {
		return machine.FetchResult{}, fmt.Errorf("machine: %q names no host", u.Redacted())
	}
	res, err := m.Exec(ctx, machine.ExecRequest{
		Command: fetchScript,
		Env: map[string]string{
			"TOPOS_FETCH_URL":       u.String(),
			"TOPOS_FETCH_DIR":       path.Join(m.SpillDir(), "fetch"),
			"TOPOS_FETCH_REDIRECTS": strconv.Itoa(fetchRedirects),
			"TOPOS_FETCH_SECONDS":   strconv.FormatFloat(fetchTimeout.Seconds(), 'f', 3, 64),
			"TOPOS_FETCH_ACCEPT":    fetchAccept,
			"TOPOS_FETCH_AGENT":     fetchUserAgent,
			"TOPOS_FETCH_LIMIT":     strconv.FormatInt(fetchMaxBody+1, 10),
		},
		// curl holds to the fetch's own timeout; this bounds the script
		// around it.
		Timeout: fetchTimeout + 30*time.Second,
	})
	switch {
	case err != nil:
		return machine.FetchResult{}, fmt.Errorf("machine: fetch: %w", err)
	case res.Canceled:
		return machine.FetchResult{}, fmt.Errorf("machine: fetch: %w", context.Cause(ctx))
	case res.TimedOut:
		return machine.FetchResult{}, fmt.Errorf("machine: the fetch passed its %s timeout: %w", fetchTimeout, context.DeadlineExceeded)
	case res.ExitCode != 0:
		return machine.FetchResult{}, fmt.Errorf("machine: fetch: the fetch script exited %d: %s", res.ExitCode, truncate(res.Output))
	}
	return parseFetch(res.Output)
}

// parseFetch reads the fetch script's answer.
func parseFetch(out []byte) (machine.FetchResult, error) {
	if rest, ok := bytes.CutPrefix(out, []byte("error ")); ok {
		code, msg, _ := strings.Cut(string(rest), " ")
		msg = strings.TrimSpace(msg)
		if code == curlTimedOut {
			return machine.FetchResult{}, fmt.Errorf("machine: the fetch passed its %s timeout: %s: %w", fetchTimeout, msg, context.DeadlineExceeded)
		}
		return machine.FetchResult{}, fmt.Errorf("machine: fetch: curl exited %s: %s", code, msg)
	}
	rest, ok := bytes.CutPrefix(out, []byte("ok\n"))
	if !ok {
		return machine.FetchResult{}, fmt.Errorf("machine: fetch: the fetch script answered %q", truncate(out))
	}
	var head [3]string
	for i := range head {
		line, after, found := bytes.Cut(rest, []byte("\n"))
		if !found {
			return machine.FetchResult{}, fmt.Errorf("machine: fetch: the fetch script's answer ends after %d lines", i)
		}
		head[i], rest = string(line), after
	}
	status, err := strconv.Atoi(head[0])
	if err != nil {
		return machine.FetchResult{}, fmt.Errorf("machine: fetch: the status %q: %w", head[0], err)
	}
	res := machine.FetchResult{URL: head[2], Status: status, ContentType: head[1], Body: rest}
	if int64(len(rest)) > fetchMaxBody {
		res.Body, res.Truncated = rest[:fetchMaxBody], true
	}
	return res, nil
}

var _ machine.Fetcher = (*Machine)(nil)
