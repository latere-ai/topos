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
	// ErrSourceTooLarge is a file past what a read takes, or one the
	// files API answered with a redirect, which it gives a file past its
	// inline bound and a public file.
	ErrSourceTooLarge = errors.New("filestore: the file is past what a source's read takes")
)

// ReadSource reads the file src names, a file of a person's own space
// (spec 079), at the files API whose root is root, with token as its
// bearer, through client and following no redirect: at most max bytes,
// with the Content-Type the files API answered. root is the root of a
// session's folder, under which files/ is served, and src.Version, when
// set, is read as ?version=N.
func ReadSource(ctx context.Context, client *http.Client, root, token string, src session.AttachmentSource, max int64) ([]byte, string, error) {
	u, err := url.Parse(root)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return nil, "", fmt.Errorf("filestore: the files API's root %q is not an http or https URL", root)
	case client == nil:
		return nil, "", errors.New("filestore: no HTTP client")
	case token == "":
		return nil, "", errors.New("filestore: no bearer to read a source with")
	}
	address := strings.TrimRight(root, "/") + "/files/" + url.PathEscape(src.Owner) + "/" + escapePath(src.Path)
	if src.Version > 0 {
		address += "?version=" + strconv.Itoa(src.Version)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	api := *client
	api.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := api.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("filestore: read %s of %s: %w", src.Path, src.Owner, err)
	}
	defer closeBody(resp)
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusNotFound:
		return nil, "", ErrSourceNotFound
	case resp.StatusCode/100 == 3:
		return nil, "", fmt.Errorf("%w: the files API answered %d, a redirect, which it gives a file past its inline bound or a public file", ErrSourceTooLarge, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, "", refusal("source", resp)
	case resp.ContentLength > max:
		return nil, "", fmt.Errorf("%w: the file is %d bytes, at most %d", ErrSourceTooLarge, resp.ContentLength, max)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, "", fmt.Errorf("filestore: read %s of %s: %w", src.Path, src.Owner, err)
	}
	if int64(len(data)) > max {
		return nil, "", fmt.Errorf("%w: the file is past %d bytes", ErrSourceTooLarge, max)
	}
	return data, resp.Header.Get("Content-Type"), nil
}
