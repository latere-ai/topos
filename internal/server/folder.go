// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/session"
)

// CodeFilesUnavailable is spec 068's code for a file of a session's
// folder of the files store that could not be written, read or copied.
const CodeFilesUnavailable = "files_unavailable"

// FolderRule is the create route's sentence on the folder of a files
// store an allow names (spec 068).
const FolderRule = "The authorizer's allow of session.create or session.fork may name folder {url, owner, path}, a folder of an Arca-compatible files API: url the API's root, under which files/ and uploads are served, owner the space, and path the folder in that space. " +
	"The session then keeps the files of its messages and the files its answers show or link in that folder in place of its blobs: a message's file at attachments/<event id>/<name>, and a file an answer of turn N shows or links at results/<N>/<path>, path its place in the working directory, " +
	"each written once, with the session's own token, and each record names it by ref, its path in the folder, beside blob, its digest, by which the blob route reads it. " +
	"The session answers folder {owner, path} from the first file stored there, and no folder before. " +
	"A fork copies into its own folder, on the server, each folder of its parent's that its copied events and its message name. " +
	"An allow that names a folder on a server that keeps no files store, or a folder that is not an http or https URL with no user, query or fragment, an owner, and a clean path of two segments or more, is authorizer_unavailable. " +
	"A file the files service does not take or give is files_unavailable."

// filesRefused is the refusal of a files store call that failed: a write
// the files service refused for its size as tooLarge, when one is given,
// and any other failure as files_unavailable.
func filesRefused(err error, tooLarge string, detail string) error {
	if tooLarge != "" && harness.FileTooLarge(err) {
		return &apiError{code: tooLarge, detail: detail, err: err}
	}
	return &apiError{code: CodeFilesUnavailable, detail: detail, err: err}
}

// folderOf is the folder an allow of a create or a fork names for the
// session (spec 068): nil for none, and a refusal when this server keeps
// no files store, since the authorizer asked for what it cannot do.
func (s *Server) folderOf(f *session.Folder) (*session.Folder, error) {
	if f == nil {
		return nil, nil
	}
	if s.o.Files == nil {
		return nil, &apiError{code: auth.CodeAuthorizerUnavailable, detail: "limits.folder names a folder of a files store, and this server keeps none: TOPOS_FILES_AUDIENCE is unset"}
	}
	out := *f
	out.Stored = false
	return &out, nil
}

// folderAnswer is a session's folder as the API answers it (spec 068):
// its space and its path, without the files API's address, once the
// session has stored a file there, and nil before.
func folderAnswer(f *session.Folder) *session.Folder {
	if f == nil || !f.Stored {
		return nil
	}
	return &session.Folder{Owner: f.Owner, Path: f.Path}
}

// putAttachments writes a message's new files into the session's folder
// with the session's own token, each at its path under the message's
// event id (spec 068), and answers their records naming each by ref. A
// file the files service refuses for its size is attachment_too_large,
// and any other failure files_unavailable, so the message is refused
// before it is appended.
func (s *Server) putAttachments(ctx context.Context, sess session.Session, files []file, records []session.Attachment) error {
	if len(files) == 0 {
		return nil
	}
	store, err := s.o.Files.Write(ctx, sess)
	if err != nil {
		return filesRefused(err, "", "open the session's folder: "+err.Error())
	}
	for i, f := range files {
		if f.blob != "" {
			continue
		}
		if _, err := store.Put(ctx, records[i].Ref, f.media, int64(len(f.data)), bytes.NewReader(f.data)); err != nil {
			return filesRefused(err, CodeAttachmentTooLarge, "write "+records[i].Ref+": "+err.Error())
		}
	}
	return nil
}

// openFile answers the file of the session's folder whose digest is d,
// read with the server's own token, and its length, false when the digest
// names no file of the folder (spec 068). A digest of a session with no
// folder names none.
func (s *Server) openFile(ctx context.Context, sess session.Session, d session.Digest) (io.ReadCloser, int64, bool, error) {
	if sess.Folder == nil {
		return nil, 0, false, nil
	}
	ref, err := session.FileRefOf(ctx, s.o.Sessions, sess.ID, d)
	switch {
	case errors.Is(err, session.ErrNotFound):
		return nil, 0, false, nil
	case err != nil:
		return nil, 0, false, err
	case s.o.Files == nil:
		return nil, 0, false, &apiError{code: CodeFilesUnavailable, detail: "the file is in the session's folder of a files store, and this server keeps none: TOPOS_FILES_AUDIENCE is unset"}
	}
	store, err := s.o.Files.Read(ctx, sess)
	if err != nil {
		return nil, 0, false, filesRefused(err, "", "open the session's folder: "+err.Error())
	}
	rc, n, err := store.Open(ctx, ref)
	switch {
	case errors.Is(err, harness.ErrFileNotFound):
		return nil, 0, false, &apiError{code: CodeNotFound, detail: "the session's folder holds nothing at " + ref, err: err}
	case err != nil:
		return nil, 0, false, filesRefused(err, "", "read "+ref+": "+err.Error())
	}
	return rc, n, true, nil
}

// forkFiles is what a fork does with the files of its parent's folder
// (spec 068): the folders its copied events and its message name, each
// copied whole on the server into the fork's folder, and its message's
// new files, put there under the message's event id. Nil when the fork
// names no file of a folder.
type forkFiles struct {
	from  session.Folder
	dirs  []string
	files []file
	refs  []session.Attachment
}

// forkFilesOf reads what a fork of parent copies of its folder: the
// directories the copied events name by ref and those of refs, the files
// the fork's message names by blob. A fork that names a file of its
// parent's folder needs a folder of its own.
func forkFilesOf(parent, fork session.Session, events []session.Event, refs []string) (*forkFiles, error) {
	var dirs []string
	add := func(ref string) {
		if d := session.RefDir(ref); d != "" && !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	for _, e := range events {
		for _, f := range e.Files() {
			add(f.Ref)
		}
	}
	for _, ref := range refs {
		add(ref)
	}
	if len(dirs) == 0 {
		return nil, nil
	}
	switch {
	case parent.Folder == nil:
		return nil, refuse(CodeFilesUnavailable, "the session forked names files of a folder it does not record")
	case fork.Folder == nil:
		return nil, refuse(CodeFilesUnavailable, "the session forked keeps files in a folder of the files store, and the fork's allow names no folder for it")
	}
	return &forkFiles{from: *parent.Folder, dirs: dirs}, nil
}

// prepare copies the fork's folders from its parent's and writes its
// message's new files, with the fork's own token, once the fork's record
// exists and before its log is written. A folder the parent's holds
// nothing under is gone from the parent too, and is skipped.
func (s *Server) prepareFork(ff *forkFiles) func(context.Context, session.Session) error {
	return func(ctx context.Context, fork session.Session) error {
		if ff == nil || (len(ff.dirs) == 0 && len(ff.files) == 0) {
			return nil
		}
		store, err := s.o.Files.Write(ctx, fork)
		if err != nil {
			return filesRefused(err, "", "open the fork's folder: "+err.Error())
		}
		for _, dir := range ff.dirs {
			err := store.CopyFolder(ctx, ff.from, dir)
			switch {
			case errors.Is(err, harness.ErrFileNotFound):
				s.o.Log.WarnContext(ctx, "a fork's folder names nothing in its parent's", "session", fork.ID, "dir", dir)
			case err != nil:
				return filesRefused(err, "", "copy "+dir+" of "+ff.from.Path+": "+err.Error())
			}
		}
		for i, f := range ff.files {
			if _, err := store.Put(ctx, ff.refs[i].Ref, f.media, int64(len(f.data)), bytes.NewReader(f.data)); err != nil {
				return filesRefused(err, CodeAttachmentTooLarge, "write "+ff.refs[i].Ref+": "+err.Error())
			}
		}
		return nil
	}
}
