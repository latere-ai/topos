// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package filestore is the backend of a session's files store (spec
// 068): Arca, a client of an Arca-compatible files API for one session's
// folder, which implements harness.Files. It writes a file of at most
// InlineBytes in one PUT, create-only or in place of what is there, and a
// larger one through an upload session whose parts go straight to the
// bucket, reads a file and follows the files API's redirect to a
// presigned address itself, without the bearer, deletes a file for good,
// and copies a folder on the server. Every request to the files
// API carries the bearer its token function answers; no request to a
// presigned address carries one.
package filestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/session"
)

// InlineBytes is the most bytes a put sends in one request, an
// Arca-compatible files API's single-request bound by default; a larger
// file goes through an upload session. The files API does not publish its
// own bound, so an installation that lowered it refuses a put between the
// two as object_too_large.
const InlineBytes = 16 << 20

// maxErrorBody bounds the bytes of a refusal that are read.
const maxErrorBody = 64 << 10

// Token answers the bearer of a request to the files API.
type Token func(ctx context.Context) (string, error)

// Arca is one session's folder at an Arca-compatible files API.
type Arca struct {
	folder session.Folder
	token  Token
	// api carries the requests to the files API and follows no
	// redirect, which a read follows itself.
	api *http.Client
}

var _ harness.Files = (*Arca)(nil)

// New is the folder f, reached through client with the bearer token
// answers. f's URL is the files API's root, under which files/ and
// uploads are served.
func New(f session.Folder, token Token, client *http.Client) (*Arca, error) {
	u, err := url.Parse(f.URL)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return nil, fmt.Errorf("filestore: the folder's url %q is not an http or https URL", f.URL)
	case f.Owner == "" || f.Path == "":
		return nil, errors.New("filestore: the folder names no owner or no path")
	case token == nil || client == nil:
		return nil, errors.New("filestore: no token or no HTTP client")
	}
	api := *client
	api.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	f.URL = strings.TrimRight(f.URL, "/")
	return &Arca{folder: f, token: token, api: &api}, nil
}

// escapePath percent-encodes each segment of a slash-separated path.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// fileURL is the address of the path p of the folder's space.
func (a *Arca) fileURL(p string) string {
	return a.folder.URL + "/files/" + url.PathEscape(a.folder.Owner) + "/" + escapePath(p)
}

// at is the path of ref in the folder's space.
func (a *Arca) at(ref string) string { return a.folder.Path + "/" + ref }

// send sends one request to the files API with the bearer.
// A body the request never reaches the transport with is closed here, as
// the transport closes one it took.
func (a *Arca) send(ctx context.Context, method, u string, body io.Reader, size int64, header http.Header) (*http.Response, error) {
	unsent := func(err error) (*http.Response, error) {
		if c, ok := body.(io.Closer); ok {
			err = errors.Join(err, c.Close())
		}
		return nil, err
	}
	tok, err := a.token(ctx)
	if err != nil {
		return unsent(fmt.Errorf("filestore: the files store's token: %w", err))
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return unsent(err)
	}
	maps.Copy(req.Header, header)
	if body != nil {
		req.ContentLength = size
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return a.api.Do(req)
}

// sendJSON sends a JSON body to the files API.
func (a *Arca) sendJSON(ctx context.Context, method, u string, in any, header http.Header) (*http.Response, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	if header == nil {
		header = http.Header{}
	}
	header.Set("Content-Type", "application/json")
	return a.send(ctx, method, u, bytes.NewReader(b), int64(len(b)), header)
}

// refusal reads a refusal of the files API as the error of op: a taken
// path as harness.ErrFileExists, a missing one as harness.ErrFileNotFound,
// and anything else as a harness.FileError with the service's code.
func refusal(op string, resp *http.Response) error {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil {
		return fmt.Errorf("filestore: %s answered %d, and its body could not be read: %w", op, resp.StatusCode, err)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Detail string `json:"detail"`
			} `json:"details"`
		} `json:"error"`
	}
	e := &harness.FileError{Op: op, Status: resp.StatusCode, Code: "unknown", Detail: strings.TrimSpace(string(raw))}
	if json.Unmarshal(raw, &env) == nil && env.Error.Code != "" {
		e.Code, e.Detail = env.Error.Code, env.Error.Details.Detail
	}
	switch resp.StatusCode {
	case http.StatusPreconditionFailed:
		return fmt.Errorf("%w: %w", harness.ErrFileExists, e)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %w", harness.ErrFileNotFound, e)
	}
	return e
}

// closeBody drains nothing and closes a response's body; its error is
// the connection's, and no answer is lost by it.
func closeBody(resp *http.Response) { _ = resp.Body.Close() }

// counted reads r, hashing and counting what it reads, and notes the
// end of r.
type counted struct {
	r   io.Reader
	mu  sync.Mutex
	h   io.Writer
	n   int64
	eof bool
}

func (c *counted) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n += int64(n)
	c.eof = c.eof || errors.Is(err, io.EOF)
	// A hash's Write never fails.
	_, _ = c.h.Write(p[:n])
	return n, err
}

// failed is err of a write of src, which named size bytes: a source that
// ended before them is harness.ErrFileSizeChanged.
func (c *counted) failed(err error, ref string, size int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.eof && c.n < size {
		return fmt.Errorf("%w: %s ended at %d of the %d bytes its write named: %w", harness.ErrFileSizeChanged, ref, c.n, size, err)
	}
	return fmt.Errorf("filestore: write %s: %w", ref, err)
}

// Put writes exactly size bytes of r at ref, create-only: in one PUT at
// or below InlineBytes, through an upload session above it. The bytes are
// hashed as they are sent, and a source of another size fails the write
// with harness.ErrFileSizeChanged.
func (a *Arca) Put(ctx context.Context, ref, mediaType string, size int64, r io.Reader) (session.Digest, error) {
	return a.put(ctx, ref, mediaType, size, r, true)
}

// Overwrite is Put with no If-None-Match: the file at ref, if any, is
// replaced. Whether the files API keeps the replaced content as a version
// is its authorizer's to say; an installation that keeps a session's
// folder as its latest state answers versions false there.
func (a *Arca) Overwrite(ctx context.Context, ref, mediaType string, size int64, r io.Reader) (session.Digest, error) {
	return a.put(ctx, ref, mediaType, size, r, false)
}

// put writes the file at ref, create-only when create is set.
func (a *Arca) put(ctx context.Context, ref, mediaType string, size int64, r io.Reader, create bool) (session.Digest, error) {
	if err := session.CheckRef(ref); err != nil {
		return "", err
	}
	if size < 0 {
		return "", fmt.Errorf("filestore: a put of %d bytes", size)
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	h := sha256.New()
	src := &counted{r: r, h: h}
	// A create-only write names If-None-Match: *, on the PUT and on an
	// upload's completion.
	cond := http.Header{}
	if create {
		cond.Set("If-None-Match", "*")
	}
	var err error
	if size <= InlineBytes {
		err = a.putOnce(ctx, ref, mediaType, size, src, cond)
	} else {
		err = a.putParts(ctx, ref, mediaType, size, src, cond)
	}
	if err != nil {
		return "", err
	}
	// A source that holds more than its size was cut at it; the file in
	// the store is the first size bytes, and the write is not the file.
	var one [1]byte
	if n, _ := io.ReadFull(r, one[:]); src.n != size || n > 0 {
		return "", fmt.Errorf("%w: %s took %d bytes of a source that holds more", harness.ErrFileSizeChanged, ref, size)
	}
	return session.Digest("sha256:" + hex.EncodeToString(h.Sum(nil))), nil
}

// sentBody is a request body of exactly n bytes of a source that says
// when the transport is done with it. net/http may go on reading a body
// after the response arrives, as when a refusal comes before the body is
// sent, and closes it once it has stopped; a write waits for that before
// it reads the source's count or hands the source back, so the source is
// read by one goroutine at a time.
type sentBody struct {
	r    io.Reader
	once sync.Once
	done chan struct{}
}

// body is a request body of exactly n bytes of src.
func body(src io.Reader, n int64) *sentBody {
	b := &sentBody{r: io.LimitReader(src, n), done: make(chan struct{})}
	if n == 0 {
		b.r = http.NoBody
		b.once.Do(func() { close(b.done) })
	}
	return b
}

// request is the body as a request carries it: http.NoBody for none,
// since a request with a body and a length of 0 is sent chunked.
func (b *sentBody) request() io.Reader {
	if b.r == http.NoBody {
		return http.NoBody
	}
	return b
}

func (b *sentBody) Read(p []byte) (int, error) { return b.r.Read(p) }

// Close records that the transport is done with the body.
func (b *sentBody) Close() error {
	b.once.Do(func() { close(b.done) })
	return nil
}

// sent waits until the transport is done with the body, or ctx ends.
func (b *sentBody) sent(ctx context.Context) {
	select {
	case <-b.done:
	case <-ctx.Done():
	}
}

// putOnce writes the file in one PUT with the headers of cond.
func (a *Arca) putOnce(ctx context.Context, ref, mediaType string, size int64, src *counted, cond http.Header) error {
	b := body(src, size)
	h := cond.Clone()
	h.Set("Content-Type", mediaType)
	resp, err := a.send(ctx, http.MethodPut, a.fileURL(a.at(ref)), b.request(), size, h)
	if err != nil {
		b.sent(ctx)
		return src.failed(err, ref, size)
	}
	if resp.StatusCode/100 != 2 {
		err := refusal("put", resp)
		closeBody(resp)
		b.sent(ctx)
		return err
	}
	closeBody(resp)
	b.sent(ctx)
	return nil
}

// uploadSession is an upload session as the files API opens it.
type uploadSession struct {
	ID       string   `json:"id"`
	PartSize int64    `json:"part_size"`
	PartURLs []string `json:"part_urls"`
}

// putParts writes the file through an upload session: its parts straight
// to the presigned addresses, with no bearer, and a completion with the
// headers of cond. A failure after the session opened aborts it.
func (a *Arca) putParts(ctx context.Context, ref, mediaType string, size int64, src *counted, cond http.Header) (err error) {
	resp, err := a.sendJSON(ctx, http.MethodPost, a.folder.URL+"/uploads",
		map[string]any{"owner": a.folder.Owner, "path": a.at(ref), "size": size, "content_type": mediaType}, nil)
	if err != nil {
		return fmt.Errorf("filestore: open an upload of %s: %w", ref, err)
	}
	if resp.StatusCode/100 != 2 {
		defer closeBody(resp)
		return refusal("upload", resp)
	}
	var up uploadSession
	derr := json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&up)
	closeBody(resp)
	if derr != nil {
		return fmt.Errorf("filestore: the upload of %s answered no session: %w", ref, derr)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, a.abort(context.WithoutCancel(ctx), up.ID))
		}
	}()
	if up.ID == "" || up.PartSize <= 0 || int64(len(up.PartURLs)) != (size+up.PartSize-1)/up.PartSize {
		return fmt.Errorf("filestore: the upload of %s answered %d parts of %d bytes for %d bytes", ref, len(up.PartURLs), up.PartSize, size)
	}
	type part struct {
		N    int    `json:"n"`
		ETag string `json:"etag"`
	}
	parts := make([]part, 0, len(up.PartURLs))
	for i, u := range up.PartURLs {
		n := min(up.PartSize, size-int64(i)*up.PartSize)
		etag, err := a.putPart(ctx, u, body(src, n), n)
		if err != nil {
			return src.failed(err, ref, size)
		}
		parts = append(parts, part{N: i + 1, ETag: etag})
	}
	resp, err = a.sendJSON(ctx, http.MethodPost, a.folder.URL+"/uploads/"+url.PathEscape(up.ID)+"/complete",
		map[string]any{"parts": parts}, cond.Clone())
	if err != nil {
		return fmt.Errorf("filestore: complete the upload of %s: %w", ref, err)
	}
	defer closeBody(resp)
	if resp.StatusCode/100 != 2 {
		return refusal("complete", resp)
	}
	return nil
}

// presigned checks an address the files API handed out: http or https
// alone.
func presigned(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("filestore: the files API handed out %q, not an http or https address", u.Redacted())
	}
	return nil
}

// putPart writes one part to its presigned address, with no bearer, and
// answers the ETag the bucket gave it.
func (a *Arca) putPart(ctx context.Context, address string, b *sentBody, n int64) (string, error) {
	u, err := url.Parse(address)
	if err != nil {
		return "", fmt.Errorf("filestore: a part's address: %w", err)
	}
	if err := presigned(u); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), b)
	if err != nil {
		return "", err
	}
	req.ContentLength = n
	resp, err := a.api.Do(req)
	if err != nil {
		b.sent(ctx)
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		err := refusal("part", resp)
		closeBody(resp)
		b.sent(ctx)
		return "", err
	}
	closeBody(resp)
	b.sent(ctx)
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", errors.New("filestore: the bucket answered a part with no ETag")
	}
	return etag, nil
}

// abort aborts an upload session.
func (a *Arca) abort(ctx context.Context, id string) error {
	resp, err := a.send(ctx, http.MethodDelete, a.folder.URL+"/uploads/"+url.PathEscape(id), nil, 0, nil)
	if err != nil {
		return fmt.Errorf("filestore: abort the upload %s: %w", id, err)
	}
	defer closeBody(resp)
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return refusal("abort", resp)
	}
	return nil
}

// Open reads the file at ref. A redirect is followed once, to an http or
// https address, without the bearer, since a presigned address carries
// its own credential.
func (a *Arca) Open(ctx context.Context, ref string) (io.ReadCloser, int64, error) {
	if err := session.CheckRef(ref); err != nil {
		return nil, 0, err
	}
	resp, err := a.send(ctx, http.MethodGet, a.fileURL(a.at(ref)), nil, 0, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("filestore: read %s: %w", ref, err)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return resp.Body, resp.ContentLength, nil
	case resp.StatusCode/100 != 3:
		defer closeBody(resp)
		return nil, 0, refusal("read", resp)
	}
	loc, err := resp.Location()
	closeBody(resp)
	if err != nil {
		return nil, 0, fmt.Errorf("filestore: the read of %s redirected nowhere: %w", ref, err)
	}
	if err := presigned(loc); err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loc.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err = a.api.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("filestore: read %s at its presigned address: %w", ref, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer closeBody(resp)
		return nil, 0, refusal("presigned read", resp)
	}
	return resp.Body, resp.ContentLength, nil
}

// Delete removes the file at ref for good, permanent=1 so it is not kept
// in the space's trash: a redaction's file and a working directory's
// removed file are gone, not restorable. A file already gone is no error.
func (a *Arca) Delete(ctx context.Context, ref string) error {
	if err := session.CheckRef(ref); err != nil {
		return err
	}
	resp, err := a.send(ctx, http.MethodDelete, a.fileURL(a.at(ref))+"?permanent=1", nil, 0, nil)
	if err != nil {
		return fmt.Errorf("filestore: delete %s: %w", ref, err)
	}
	defer closeBody(resp)
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return refusal("delete", resp)
	}
	return nil
}

// CopyFolder copies the folder dir of from to the same place in this
// folder, in one recursive copy on the server. from is a folder of the
// same space of the same files API, since a copy never crosses a space.
func (a *Arca) CopyFolder(ctx context.Context, from session.Folder, dir string) error {
	switch {
	case strings.TrimRight(from.URL, "/") != a.folder.URL || from.Owner != a.folder.Owner:
		return fmt.Errorf("filestore: the folder %s of %s at %s is not in this folder's space", from.Path, from.Owner, from.URL)
	case dir == "" || strings.HasPrefix(dir, "/") || strings.Contains("/"+dir+"/", "/../") || strings.Contains("/"+dir+"/", "//"):
		return fmt.Errorf("filestore: %q names no folder", dir)
	}
	resp, err := a.sendJSON(ctx, http.MethodPost, a.fileURL(from.Path+"/"+dir), map[string]any{"copy_to": a.at(dir), "recursive": true}, nil)
	if err != nil {
		return fmt.Errorf("filestore: copy %s: %w", dir, err)
	}
	defer closeBody(resp)
	if resp.StatusCode/100 != 2 {
		return refusal("copy", resp)
	}
	return nil
}
