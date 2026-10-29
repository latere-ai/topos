// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arca

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
	"regexp"
	"strings"
	"time"

	"latere.ai/x/topos/memory"
	"latere.ai/x/topos/session"
)

// RequestTimeout bounds one request to the files store; a request past
// it is Unreachable.
const RequestTimeout = 30 * time.Second

// maxErrorBody bounds the bytes of a refusal that are read.
const maxErrorBody = 64 << 10

// listPage is how many entries one listing request asks for, the files
// API's largest page, and maxListingBody bounds the bytes of one page.
const (
	listPage       = 1000
	maxListingBody = 4 << 20
)

// Token answers the bearer of a request to the files store: the
// session's own token for TOPOS_FILES_AUDIENCE, asked at each request so
// a renewed token reaches the next one.
type Token func(ctx context.Context) (string, error)

// escapePath percent-encodes each segment of a slash-separated path.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// fileURL is the address of rel in f, or of f itself for an empty rel.
func fileURL(f session.MemoryFolder, rel string) string {
	p := f.Path
	if rel != "" {
		p += "/" + rel
	}
	return strings.TrimRight(f.URL, "/") + "/files/" + url.PathEscape(f.Owner) + "/" + escapePath(p)
}

// answer is one answer of the files store, read whole: every answer of a
// memory request is at most memory.MaxFileBytes, or a refusal. length is the
// length the answer names, -1 for none.
type answer struct {
	status int
	header http.Header
	length int64
	body   []byte
}

// noBody is the limit of a request whose answer has no body, a HEAD's.
const noBody = -1

// send sends one request to the files store with the session's bearer,
// under RequestTimeout, and reads its answer whole, at most limit bytes
// of a 2xx body; a longer one is TooLarge, and noBody reads none. A 3xx to a presigned address
// is followed without the bearer, since the address carries its own
// credential, and keeps the files store's headers, its ETag among them.
func (m *Machine) send(ctx context.Context, method, u string, body []byte, header http.Header, limit int64, p string) (answer, error) {
	tok, err := m.token(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return answer{}, ctx.Err()
		}
		return answer{}, &memory.Unreachable{Detail: "the session's token for the files store could not be had", Err: err}
	}
	rctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(rctx, method, u, r)
	if err != nil {
		return answer{}, fmt.Errorf("memory: %s %s: %w", method, p, err)
	}
	maps.Copy(req.Header, header)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := m.api.Do(req)
	if err != nil {
		return answer{}, unreached(ctx, err, "")
	}
	defer closeBody(resp)
	if resp.StatusCode/100 == 3 && method == http.MethodGet {
		return m.presigned(ctx, rctx, resp, limit, p)
	}
	return readAnswer(ctx, resp, limit, p)
}

// presigned follows the files store's redirect of a read to the address
// it names, with no bearer, and answers the object's bytes under the
// redirect's headers.
func (m *Machine) presigned(ctx, rctx context.Context, redirect *http.Response, limit int64, p string) (answer, error) {
	loc, err := redirect.Location()
	if err != nil || (loc.Scheme != "http" && loc.Scheme != "https") || loc.Host == "" {
		return answer{}, &memory.Unreachable{Detail: "the files store redirected a read to no http or https address"}
	}
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, loc.String(), nil)
	if err != nil {
		return answer{}, &memory.Unreachable{Detail: "the files store redirected a read to " + unsigned(loc)}
	}
	resp, err := m.api.Do(req)
	if err != nil {
		return answer{}, unreached(ctx, err, unsigned(loc))
	}
	defer closeBody(resp)
	a, err := readAnswer(ctx, resp, limit, p)
	if err != nil {
		return answer{}, err
	}
	if a.status/100 == 2 {
		a.header = redirect.Header
	}
	return a, nil
}

// readAnswer reads an answer whole: a 2xx body of at most limit bytes,
// and at most maxErrorBody of any other.
func readAnswer(ctx context.Context, resp *http.Response, limit int64, p string) (answer, error) {
	if limit == noBody {
		return answer{status: resp.StatusCode, header: resp.Header, length: resp.ContentLength}, nil
	}
	n := int64(maxErrorBody)
	if resp.StatusCode/100 == 2 {
		if resp.ContentLength > limit {
			return answer{}, &memory.TooLarge{Path: p, Size: resp.ContentLength}
		}
		n = limit
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, n+1))
	if err != nil {
		return answer{}, unreached(ctx, err, "")
	}
	if resp.StatusCode/100 == 2 && int64(len(body)) > limit {
		return answer{}, &memory.TooLarge{Path: p, Size: int64(len(body))}
	}
	return answer{status: resp.StatusCode, header: resp.Header, length: resp.ContentLength, body: body}, nil
}

// unreached is a request that failed on its way: the call's own context
// ending is that context's error, so the harness tells a cancel apart,
// and anything else is Unreachable. A failure at a presigned address is
// named by its scheme and host alone, since its query is a credential.
func unreached(ctx context.Context, err error, at string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &memory.Unreachable{Detail: fmt.Sprintf("the files store did not answer within %s", RequestTimeout)}
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	detail := "the request to the files store failed"
	if at != "" {
		detail = "the read at " + at + " failed"
	}
	return &memory.Unreachable{Detail: detail, Err: err}
}

// unsigned is a presigned address as an error names it: its scheme and
// host alone.
func unsigned(u *url.URL) string { return u.Scheme + "://" + u.Host }

// closeBody closes a response's body; its error is the connection's, and
// no answer is lost by it.
func closeBody(resp *http.Response) { _ = resp.Body.Close() }

// failed is an answer that is not a 2xx as the error of op on p: a 412 is
// memory.ErrChanged, a 404 the file's absence, a 4xx with the store's error
// envelope a Refusal, and a 5xx or a 4xx whose body does not decode
// Unreachable.
func failed(op, p string, a answer) error {
	switch {
	case a.status == http.StatusPreconditionFailed:
		return fmt.Errorf("%w: %s", memory.ErrChanged, p)
	case a.status == http.StatusNotFound:
		return notExist(op, p)
	case a.status/100 != 4:
		return &memory.Unreachable{Detail: fmt.Sprintf("the files store answered the %s with %d", op, a.status)}
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				Detail string `json:"detail"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(a.body, &env) != nil || env.Error.Code == "" || env.Error.Message == "" {
		return &memory.Unreachable{Detail: fmt.Sprintf("the files store answered the %s with %d and a body that does not decode", op, a.status)}
	}
	reason, words := splitReason(env.Error.Details.Detail)
	return &memory.Refusal{Op: op, Status: a.status, Code: env.Error.Code, Message: env.Error.Message, Reason: reason, Detail: words}
}

// etagOf is the version an answer names, its ETag without the quotes:
// the file's checksum.
func etagOf(h http.Header) string {
	return strings.Trim(strings.TrimPrefix(h.Get("ETag"), "W/"), `"`)
}

// listing is one page of the files store's listing of a folder.
type listing struct {
	Entries []struct {
		Path     string `json:"path"`
		Size     int64  `json:"size"`
		Checksum string `json:"checksum"`
		Modified string `json:"modified"`
	} `json:"entries"`
	NextCursor string `json:"next_cursor"`
}

// listed is one file of a folder's listing.
type listed struct {
	rel      string
	size     int64
	checksum string
	modified time.Time
}

// list reads the files of f, a page at a time, at most memory.MaxListed of them,
// and reports whether the folder holds more.
func (m *Machine) list(ctx context.Context, f session.MemoryFolder) ([]listed, bool, error) {
	var out []listed
	cursor := ""
	for {
		q := url.Values{"list": {"1"}, "limit": {fmt.Sprint(listPage)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		a, err := m.send(ctx, http.MethodGet, fileURL(f, "")+"?"+q.Encode(), nil, nil, maxListingBody, memory.Root+"/"+f.Name)
		if err != nil {
			return nil, false, err
		}
		if a.status/100 != 2 {
			return nil, false, failed("listing", memory.Root+"/"+f.Name, a)
		}
		var page listing
		if err := json.Unmarshal(a.body, &page); err != nil {
			return nil, false, &memory.Unreachable{Detail: "the files store's listing does not decode", Err: err}
		}
		for _, e := range page.Entries {
			rel, ok := strings.CutPrefix(e.Path, f.Path+"/")
			if !ok || rel == "" || !cleanRel(rel) {
				continue
			}
			if len(out) == memory.MaxListed {
				return out, true, nil
			}
			// An entry whose time does not parse sorts as the oldest in a
			// glob, which orders by time; its file is listed all the same.
			mod, err := time.Parse(time.RFC3339Nano, e.Modified)
			if err != nil {
				mod = time.Time{}
			}
			out = append(out, listed{rel: rel, size: e.Size, checksum: e.Checksum, modified: mod})
		}
		if page.NextCursor == "" {
			return out, false, nil
		}
		cursor = page.NextCursor
	}
}

// reasoned is an envelope's detail that names the authorizer's reason.
var reasoned = regexp.MustCompile(`(?s)^([a-z][a-z0-9_]*): (.*)$`)

// splitReason reads the authorizer's reason off an envelope's detail:
// the reason and its words for "<reason>: <words>", and nothing for any
// other detail, which is the store's developer detail and not a sentence
// for the model.
func splitReason(detail string) (reason, words string) {
	m := reasoned.FindStringSubmatch(detail)
	if m == nil {
		return "", ""
	}
	return m[1], m[2]
}
