// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package filestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"latere.ai/x/topos/session"
)

// The answers of ReadSource besides a file and a refusal of the files
// API.
var (
	// ErrSourceNotFound is a read the files API answered 401, 403 or 404:
	// no file there that the bearer may read, without saying which.
	ErrSourceNotFound = errors.New("filestore: the files API holds no file there that the bearer may read")
	// ErrSourceTooLarge is a file past what a read takes, by the length
	// its answer declares or by the bytes it sends.
	ErrSourceTooLarge = errors.New("filestore: the file is past what a source's read takes")
)

// Source is a file of a person's own space as ReadSource answers it: its
// bytes, a stream the caller closes, which fails with ErrSourceTooLarge
// once it would pass the read's bound; its length as the answer declared
// it, -1 for none; and the Content-Type of the answer that carried it.
type Source struct {
	Body      io.ReadCloser
	Size      int64
	MediaType string
}

// ReadSource reads the file src names, a file of a person's own space
// (spec 079), at the files API whose root is root, with token as its
// bearer, through client: at most max bytes. root is the root of a
// session's folder, under which files/ is served, and src.Version, when
// set, is read as ?version=N. A redirect, which the files API answers for
// a file past its inline bound and for a public file, is followed once,
// to an https address, without the bearer (spec 081).
func ReadSource(ctx context.Context, client *http.Client, root, token string, src session.AttachmentSource, max int64) (*Source, error) {
	u, err := url.Parse(root)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return nil, fmt.Errorf("filestore: the files API's root %q is not an http or https URL", root)
	case client == nil:
		return nil, errors.New("filestore: no HTTP client")
	case token == "":
		return nil, errors.New("filestore: no bearer to read a source with")
	}
	address := strings.TrimRight(root, "/") + "/files/" + url.PathEscape(src.Owner) + "/" + escapePath(src.Path)
	if src.Version > 0 {
		address += "?version=" + strconv.Itoa(src.Version)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	api := *client
	api.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := api.Do(req)
	if err != nil {
		return nil, fmt.Errorf("filestore: read %s of %s: %w", src.Path, src.Owner, err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusNotFound:
		closeBody(resp)
		return nil, ErrSourceNotFound
	case resp.StatusCode/100 == 3:
		s, err := presignedSource(ctx, &api, resp, max)
		if err != nil {
			return nil, fmt.Errorf("filestore: read %s of %s: %w", src.Path, src.Owner, err)
		}
		return s, nil
	case resp.StatusCode != http.StatusOK:
		defer closeBody(resp)
		return nil, refusal("source", resp)
	}
	return sourceOf(resp, max)
}

// sourceOf is the answer resp carries as a Source held to max, its body
// closed when the length it declares is past max.
func sourceOf(resp *http.Response, max int64) (*Source, error) {
	if resp.ContentLength > max {
		closeBody(resp)
		return nil, fmt.Errorf("%w: the file is %d bytes, at most %d", ErrSourceTooLarge, resp.ContentLength, max)
	}
	return &Source{Body: &bounded{rc: resp.Body, left: max, max: max}, Size: resp.ContentLength, MediaType: resp.Header.Get("Content-Type")}, nil
}

// presignedSource follows the files API's redirect of a source's read
// once, closing redirect's body, and answers the file held to max: to an
// https address alone, since the address carries its credential in its
// query and the person's file in its answer, and with a request of its
// own that carries none of the first's headers, so the sender's bearer
// never reaches another host. An answer that is not 200, a second
// redirect among them, is the files service's failure.
func presignedSource(ctx context.Context, api *http.Client, redirect *http.Response, max int64) (*Source, error) {
	loc, err := redirect.Location()
	closeBody(redirect)
	switch {
	case err != nil:
		return nil, fmt.Errorf("the files API answered %d and named no address: %w", redirect.StatusCode, err)
	case loc.Scheme != "https" || loc.Host == "":
		return nil, fmt.Errorf("the files API redirected to %q, not an https address", loc.Redacted())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loc.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := api.Do(req)
	if err != nil {
		return nil, fmt.Errorf("at its presigned address: %w", err)
	}
	switch {
	case resp.StatusCode/100 == 3:
		closeBody(resp)
		return nil, fmt.Errorf("the presigned address answered %d, a second redirect, which a read does not follow", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		defer closeBody(resp)
		return nil, refusal("presigned source", resp)
	}
	return sourceOf(resp, max)
}

// bounded is a source's body held to max bytes as it is read: a byte past
// them fails the read with ErrSourceTooLarge, whatever length the answer
// declared.
type bounded struct {
	rc        io.ReadCloser
	left, max int64
}

func (b *bounded) Read(p []byte) (int, error) {
	if b.left > 0 {
		if int64(len(p)) > b.left {
			p = p[:b.left]
		}
		n, err := b.rc.Read(p)
		b.left -= int64(n)
		return n, err
	}
	// One byte more than the bound tells a longer body from one that
	// ends at it.
	var one [1]byte
	n, err := b.rc.Read(one[:])
	if n > 0 {
		return 0, fmt.Errorf("%w: the file is past %d bytes", ErrSourceTooLarge, b.max)
	}
	return 0, err
}

func (b *bounded) Close() error { return b.rc.Close() }
