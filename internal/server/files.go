// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/machine"
)

// MaxFileBytes bounds a file GET /sessions/{id}/files answers (spec
// 044): a larger one is refused file_too_large, never cut short.
const MaxFileBytes = 10 << 20

// The codes of spec 044.
const (
	// CodeFileUnavailable is a file of a session whose machine is not
	// running now, so nothing in it can be read until the session runs
	// it again.
	CodeFileUnavailable = "file_unavailable"
	// CodeFileTooLarge is a file past MaxFileBytes.
	CodeFileTooLarge = "file_too_large"
)

// filePolicy is the Content-Security-Policy of a file's answer: a
// document opened from it runs in a unique origin with no scripts,
// plugins or forms, and loads nothing, so the bytes an agent wrote never
// act as a page of the API's origin, even when a browser opens the
// address directly.
const filePolicy = "sandbox; default-src 'none'"

// sniffBytes is how much of a file names its media type when its
// extension does not: what http.DetectContentType reads.
const sniffBytes = 512

// file is GET /sessions/{id}/files?path=: one file of the session's
// working directory as it is now (spec 044), asked as session.read. The
// path is absolute, as a write or edit result's meta names it, or
// relative to the working directory. The file is read through the
// server's Workspaces, which never starts or creates a machine, and is
// answered as a download: its media type, its length, an attachment
// disposition with its name, nosniff and a sandboxing policy.
func (c *call) file() error {
	s, err := c.session(authorizer.ActionSessionRead, nil)
	if err != nil {
		return err
	}
	paths := c.r.URL.Query()["path"]
	if len(paths) != 1 || paths[0] == "" {
		return refuse(CodeInvalidRequest, "exactly one path is required")
	}
	p := paths[0]
	if c.s.o.Workspaces == nil {
		return refuse(CodeFileUnavailable, "this server reads no session's working directory")
	}
	ctx := c.r.Context()
	ws, err := c.s.o.Workspaces(ctx, s)
	if err != nil {
		return fileErr(p, err)
	}
	fi, err := ws.Stat(ctx, p)
	if err != nil {
		return fileErr(p, err)
	}
	switch {
	case fi.IsDir:
		return refuse(CodeInvalidRequest, "%s is a directory; the route reads one file", p)
	case fi.Size > MaxFileBytes:
		e := refuse(CodeFileTooLarge, "%s is %d bytes, at most %d", p, fi.Size, MaxFileBytes)
		e.details = map[string]any{"size": fi.Size, "limit": MaxFileBytes}
		return e
	}
	rc, err := ws.ReadFile(ctx, p)
	if err != nil {
		return fileErr(p, err)
	}
	defer func() {
		if err := rc.Close(); err != nil {
			c.s.o.Log.WarnContext(ctx, "close a session's file", "session", s.ID, "path", p, "err", err)
		}
	}()
	head := make([]byte, min(fi.Size, sniffBytes))
	if _, err := io.ReadFull(rc, head); err != nil {
		return err
	}
	name := path.Base(strings.ReplaceAll(fi.Path, `\`, "/"))
	if fi.Path == "" {
		name = path.Base(p)
	}
	h := c.w.Header()
	h.Set("Content-Type", mediaOf(name, head))
	h.Set("Content-Length", strconv.FormatInt(fi.Size, 10))
	h.Set("Content-Disposition", disposition(name))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", filePolicy)
	h.Set("Cache-Control", "no-store")
	c.w.WriteHeader(http.StatusOK)
	if c.r.Method == http.MethodHead {
		return nil
	}
	if _, err := c.w.Write(head); err != nil {
		return err
	}
	// A file that grew since its stat is answered at the length the
	// header promised; one that shrank ends the answer early, which the
	// client sees as a short body.
	_, err = io.CopyN(c.w, rc, fi.Size-int64(len(head)))
	return err
}

// fileErr is a read's failure as the refusal it answers.
func fileErr(p string, err error) error {
	switch {
	case errors.Is(err, machine.ErrNotRunning):
		return &apiError{code: CodeFileUnavailable, detail: err.Error(), err: err}
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission):
		return &apiError{code: CodeNotFound, detail: "no file " + p, err: err}
	case errors.Is(err, machine.ErrOutside):
		return &apiError{code: CodeInvalidRequest, detail: p + " is outside the session's working directory", err: err}
	case errors.Is(err, machine.ErrDenied):
		return &apiError{code: CodeInvalidRequest, detail: p + " holds credentials, which no reader of the session reads", err: err}
	}
	return err
}

// mediaOf is a file's media type: its extension's, and otherwise the one
// its first bytes give.
func mediaOf(name string, head []byte) string {
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return http.DetectContentType(head)
}

// disposition labels the answer a download of name. A name the header
// cannot carry, one a control character or a bidirectional mark is in,
// is left out, and the client names the file.
func disposition(name string) string {
	if d := mime.FormatMediaType("attachment", map[string]string{"filename": name}); d != "" && checkName(name) == nil {
		return d
	}
	return "attachment"
}
