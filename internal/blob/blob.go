// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package blob is the blob store of spec 014 that TOPOS_BLOB_URL names:
// the bodies of a session's blobs in a directory, file:///<path>, as
// <path>/<session>/<hex>, or in an S3 compatible object store,
// s3://<host>/<bucket>/<prefix>, as the object <prefix>/<session>/<hex>.
// Both are session.Blobs, which the session stores write through.
package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"latere.ai/x/pkg/s3"

	"latere.ai/x/topos/session"
)

// DefaultRegion is the region an s3:// URL signs for when it names none
// with ?region=.
const DefaultRegion = "us-east-1"

// Open returns the blob store raw names, or nil for an empty URL, which
// keeps blobs in the session store itself. An s3:// store is reached
// over TLS, path-style, with the access and secret keys, through client.
func Open(raw, accessKey, secretKey string, client *http.Client) (session.Blobs, error) {
	if raw == "" {
		return nil, nil
	}
	u, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "file" {
		return NewDir(u.Path), nil
	}
	bucket, prefix, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	region := u.Query().Get("region")
	if region == "" {
		region = DefaultRegion
	}
	c, err := s3.New("https://"+u.Host, region, bucket, accessKey, secretKey, s3.WithPathStyle(), s3.WithHTTPClient(client))
	if err != nil {
		return nil, fmt.Errorf("blob: %w", err)
	}
	return NewS3(c, prefix), nil
}

// Parse checks a blob store URL: file:// with an absolute path, or s3://
// with a host and a bucket.
func Parse(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("blob: %q is not a URL", raw)
	}
	switch u.Scheme {
	case "file":
		if u.Host != "" || !strings.HasPrefix(u.Path, "/") {
			return nil, fmt.Errorf("blob: %q is not file:///<absolute path>", raw)
		}
	case "s3":
		if bucket, _, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/"); u.Host == "" || bucket == "" {
			return nil, fmt.Errorf("blob: %q is not s3://<host>/<bucket>/<prefix>", raw)
		}
	default:
		return nil, fmt.Errorf("blob: %q is neither file:// nor s3://", raw)
	}
	return u, nil
}

// Dir keeps the bodies in a directory.
type Dir struct{ root string }

// NewDir is the store under root.
func NewDir(root string) *Dir { return &Dir{root: root} }

func (d *Dir) path(id string, dg session.Digest) (string, error) {
	if err := session.CheckID(session.PrefixSession, id); err != nil {
		return "", err
	}
	if !dg.Valid() {
		return "", fmt.Errorf("%w: digest %q", session.ErrInvalid, dg)
	}
	return filepath.Join(d.root, id, dg.Hex()), nil
}

// PutBlob writes the body to a temporary name, fsyncs it, renames it into
// place and fsyncs the directory; a body already there is kept.
func (d *Dir) PutBlob(_ context.Context, id string, dg session.Digest, body []byte) (err error) {
	p, err := d.path(id, dg)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("blob: create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "."+dg.Hex()+".*.tmp")
	if err != nil {
		return fmt.Errorf("blob: create a temporary file: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, removeIfThere(f.Name()))
		}
	}()
	_, werr := f.Write(body)
	if err := errors.Join(werr, f.Sync(), f.Close()); err != nil {
		return fmt.Errorf("blob: write %s: %w", p, err)
	}
	if err := os.Rename(f.Name(), p); err != nil {
		return fmt.Errorf("blob: rename %s: %w", p, err)
	}
	return syncDir(dir)
}

func (d *Dir) GetBlob(_ context.Context, id string, dg session.Digest) ([]byte, error) {
	p, err := d.path(id, dg)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: blob %s", session.ErrNotFound, dg)
	}
	if err != nil {
		return nil, fmt.Errorf("blob: read %s: %w", p, err)
	}
	return b, nil
}

func (d *Dir) DeleteBlob(_ context.Context, id string, dg session.Digest) error {
	p, err := d.path(id, dg)
	if err != nil {
		return err
	}
	return removeIfThere(p)
}

func (d *Dir) DeleteSession(_ context.Context, id string) error {
	if err := session.CheckID(session.PrefixSession, id); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(d.root, id)); err != nil {
		return fmt.Errorf("blob: remove the blobs of %s: %w", id, err)
	}
	return nil
}

func (d *Dir) Sessions(context.Context) ([]string, error) {
	entries, err := os.ReadDir(d.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("blob: list %s: %w", d.root, err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && session.CheckID(session.PrefixSession, e.Name()) == nil {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

func removeIfThere(p string) error {
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("blob: remove %s: %w", p, err)
	}
	return nil
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("blob: open %s: %w", dir, err)
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return fmt.Errorf("blob: sync %s: %w", dir, err)
	}
	return nil
}

// S3 keeps the bodies as objects under a prefix.
type S3 struct {
	c      *s3.Client
	prefix string
}

// NewS3 is the store over c, its keys under prefix.
func NewS3(c *s3.Client, prefix string) *S3 {
	return &S3{c: c, prefix: strings.Trim(prefix, "/")}
}

// dir is the key prefix of one session's bodies, or of every session's
// with an empty id.
func (s *S3) dir(id string) string {
	if s.prefix == "" {
		return id + "/"
	}
	return path.Join(s.prefix, id) + "/"
}

func (s *S3) key(id string, dg session.Digest) (string, error) {
	if err := session.CheckID(session.PrefixSession, id); err != nil {
		return "", err
	}
	if !dg.Valid() {
		return "", fmt.Errorf("%w: digest %q", session.ErrInvalid, dg)
	}
	return s.dir(id) + dg.Hex(), nil
}

func (s *S3) PutBlob(ctx context.Context, id string, dg session.Digest, body []byte) error {
	k, err := s.key(id, dg)
	if err != nil {
		return err
	}
	if _, err := s.c.PutObject(ctx, k, s3.BytesBody(body)); err != nil {
		return fmt.Errorf("blob: put %s: %w", k, err)
	}
	return nil
}

func (s *S3) GetBlob(ctx context.Context, id string, dg session.Digest) ([]byte, error) {
	k, err := s.key(id, dg)
	if err != nil {
		return nil, err
	}
	rc, _, err := s.c.GetObject(ctx, k, "")
	if errors.Is(err, s3.ErrNotFound) {
		return nil, fmt.Errorf("%w: blob %s", session.ErrNotFound, dg)
	}
	if err != nil {
		return nil, fmt.Errorf("blob: get %s: %w", k, err)
	}
	b, rerr := io.ReadAll(rc)
	if err := errors.Join(rerr, rc.Close()); err != nil {
		return nil, fmt.Errorf("blob: read %s: %w", k, err)
	}
	return b, nil
}

func (s *S3) DeleteBlob(ctx context.Context, id string, dg session.Digest) error {
	k, err := s.key(id, dg)
	if err != nil {
		return err
	}
	if err := s.c.DeleteObject(ctx, k); err != nil && !errors.Is(err, s3.ErrNotFound) {
		return fmt.Errorf("blob: delete %s: %w", k, err)
	}
	return nil
}

func (s *S3) DeleteSession(ctx context.Context, id string) error {
	if err := session.CheckID(session.PrefixSession, id); err != nil {
		return err
	}
	return s.each(ctx, s3.ListOptions{Prefix: s.dir(id)}, func(r s3.ListResult) error {
		for _, o := range r.Objects {
			if err := s.c.DeleteObject(ctx, o.Key); err != nil && !errors.Is(err, s3.ErrNotFound) {
				return fmt.Errorf("blob: delete %s: %w", o.Key, err)
			}
		}
		return nil
	})
}

func (s *S3) Sessions(ctx context.Context) ([]string, error) {
	top := ""
	if s.prefix != "" {
		top = s.prefix + "/"
	}
	var ids []string
	err := s.each(ctx, s3.ListOptions{Prefix: top, Delimiter: "/"}, func(r s3.ListResult) error {
		for _, p := range r.Prefixes {
			if id := strings.TrimSuffix(strings.TrimPrefix(p, top), "/"); session.CheckID(session.PrefixSession, id) == nil {
				ids = append(ids, id)
			}
		}
		return nil
	})
	return ids, err
}

// each walks every page of a listing.
func (s *S3) each(ctx context.Context, o s3.ListOptions, fn func(s3.ListResult) error) error {
	for {
		r, err := s.c.ListObjects(ctx, o)
		if err != nil {
			return fmt.Errorf("blob: list %s: %w", o.Prefix, err)
		}
		if err := fn(r); err != nil {
			return err
		}
		if !r.Truncated {
			return nil
		}
		switch {
		case len(r.Objects) > 0:
			o.StartAfter = r.Objects[len(r.Objects)-1].Key
		case len(r.Prefixes) > 0:
			o.StartAfter = r.Prefixes[len(r.Prefixes)-1]
		default:
			return nil
		}
	}
}

var (
	_ session.Blobs = (*Dir)(nil)
	_ session.Blobs = (*S3)(nil)
)
