// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/memory"
	"latere.ai/x/topos/session"
)

// CodeFilesUnavailable is spec 068's code for a file of a session's
// folder of the files store that could not be written, read or copied.
const CodeFilesUnavailable = harness.CodeFilesUnavailable

// FolderRule is the create route's sentence on the folder of a files
// store an allow names (spec 068).
const FolderRule = "The authorizer's allow of session.create or session.fork may name folder {url, owner, path}, a folder of an Arca-compatible files API: url the API's root, under which files/ and uploads are served, owner the space, and path the folder in that space. " +
	"The session then keeps the files of its messages and the files its answers show or link in that folder in place of its blobs: a message's file at attachments/<event id>/<name>, and a file an answer of turn N shows or links at results/<N>/<path>, path its place in the working directory, " +
	"each written once, with the session's own token, and each record names it by ref, its path in the folder, beside blob, its digest, by which the blob route reads it. " +
	"The session also keeps the latest state of its working directory there, at work/<path>, written at the end of each turn that changed files and recorded by work.saved, and a new machine is restored from it. " +
	"A file a redaction leaves no live event naming is deleted from the folder. " +
	"The session answers folder {owner, path} from the first file stored there, and no folder before. " +
	"A fork copies into its own folder, on the server, each folder of its parent's that its copied events and its message name, and its parent's work/. " +
	"An allow that names a folder on a server that keeps no files store, or a folder that is not an http or https URL with no user, query or fragment, an owner, and a clean path of two segments or more, is authorizer_unavailable. " +
	"A file the files service does not take or give is files_unavailable."

// MemoryRule is the create route's sentence on the memory folders an
// allow names (spec 093).
var MemoryRule = fmt.Sprintf("The authorizer's allow of session.create or session.fork may name memory, at most %d folders of an Arca-compatible files API, each {name, url, owner, path, access}: "+
	"name a lowercase letter and at most 31 more lowercase letters, digits or -, unique, url the API's root, owner the space of 1 to %d bytes, path the folder in that space of at most %d bytes with no empty, . or .. segment, and access read_only or read_write. "+
	"The session's read, write, edit, glob and grep reach each folder at /memory/<name>/, with the session's own token, and so does a bash command whose every program is ls, cat, head, tail, wc, grep, find, sort, uniq, cut, mkdir, rm, mv, cp, touch or a shell builtin, which runs on the runner and opens no sandbox; any other command runs in the sandbox, which does not see the folders; a write of empty content forgets a file. "+
	"A folder may name sessions {metadata} in place of url, owner and path, with access read_only: a folder of conversations, one Markdown file per other session of the session's initiator in the session's context that its at most %d metadata entry keeps (a value the sessions holding the key with it, null those without the key), one per fork tree and never the session's own, "+
	"named <yyyy-mm-dd>-<session id>.md and holding what the person and the agent said, at most %d KiB, each list asked session.list and each conversation session.read as the initiator with origin {kind: %s, session_id, folder} and a read with the conversation's metadata. "+
	"A fork's memory folders are its own allow's, and a rollout's run has none. The session answers memory [{name, access}], without the API's address, the space or the path. "+
	"An allow that names a folder of a files store on a server that keeps none, or memory that breaks a rule, is authorizer_unavailable.",
	session.MaxMemoryFolders, session.MaxMemoryOwner, session.MaxMemoryPath, session.MaxMemorySessionsMetadata, memory.MaxConversationBytes>>10, OriginMemory)

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

// memoryOf is the memory folders an allow of a create or a fork names for
// the session (spec 093): none for none, and a refusal when one is of a
// files store and this server keeps none, since the runner would have no
// credential to reach it with. A folder of conversations is served from
// the server's own log (spec 094) and needs no files store.
func (s *Server) memoryOf(fs []session.MemoryFolder) ([]session.MemoryFolder, error) {
	if len(fs) == 0 {
		return nil, nil
	}
	if s.o.Files == nil && slices.ContainsFunc(fs, func(f session.MemoryFolder) bool { return !f.Conversations() }) {
		return nil, &apiError{code: auth.CodeAuthorizerUnavailable, detail: "limits.memory names folders of a files store, and this server keeps none: TOPOS_FILES_AUDIENCE is unset"}
	}
	return slices.Clone(fs), nil
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
// before it is appended; so is a write whose bytes are not the ones its
// record names.
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
		if err := putFile(ctx, store, f, records[i]); err != nil {
			return err
		}
	}
	return nil
}

// putFile writes f at its record's ref, streamed from its data or its
// spool, and refuses a write whose digest is not the record's: the
// record names the bytes as they were read, and the folder must hold
// those.
func putFile(ctx context.Context, store harness.Files, f file, record session.Attachment) error {
	d, err := store.Put(ctx, record.Ref, f.media, f.length(), f.body())
	switch {
	case err != nil:
		return filesRefused(err, CodeAttachmentTooLarge, "write "+record.Ref+": "+err.Error())
	case d != record.Blob:
		return &apiError{code: CodeFilesUnavailable, detail: fmt.Sprintf("write %s: the folder took bytes of digest %s, and the message's record names %s", record.Ref, d, record.Blob)}
	}
	return nil
}

// dropRedactedFiles deletes from the session's folder each file that
// before, the log as it was before a redaction, recorded and that no live
// event names once the redaction is written (spec 073), with the session's
// own token, for good. The redaction is the record and stands whatever
// becomes of a delete: one that fails is logged with its ref, and its file
// stays in the folder until the installation removes the folder. A
// redaction sent again plans nothing and deletes nothing more.
func (s *Server) dropRedactedFiles(ctx context.Context, sess session.Session, before []session.Event) {
	if sess.Folder == nil || s.o.Files == nil {
		return
	}
	after, err := s.o.Sessions.Events(ctx, sess.ID, 1, 0)
	if err != nil {
		s.o.Log.ErrorContext(ctx, "the files a redaction left unnamed were not deleted: the log could not be read", "session", sess.ID, "error", err)
		return
	}
	gone := session.GoneRefs(before, after)
	if len(gone) == 0 {
		return
	}
	store, err := s.o.Files.Write(ctx, sess)
	if err != nil {
		s.o.Log.ErrorContext(ctx, "the files a redaction left unnamed were not deleted: the folder could not be opened", "session", sess.ID, "refs", gone, "error", err)
		return
	}
	for _, ref := range gone {
		if err := store.Delete(ctx, ref); err != nil {
			s.o.Log.ErrorContext(ctx, "a file a redaction left unnamed was not deleted from the session's folder", "session", sess.ID, "ref", ref, "error", err)
		}
	}
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
	// work is set when the fork copies its parent's copy of the working
	// directory, session.WorkDir (spec 073).
	work bool
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

// prepare copies the fork's folders from its parent's, its parent's copy
// of the working directory among them (spec 073), and writes its
// message's new files, with the fork's own token, once the fork's record
// exists and before its log is written. A folder the parent's holds
// nothing under is gone from the parent too, and is skipped.
func (s *Server) prepareFork(ff *forkFiles) func(context.Context, session.Session) error {
	return func(ctx context.Context, fork session.Session) error {
		if ff == nil || (len(ff.dirs) == 0 && len(ff.files) == 0 && !ff.work) {
			return nil
		}
		store, err := s.o.Files.Write(ctx, fork)
		if err != nil {
			return filesRefused(err, "", "open the fork's folder: "+err.Error())
		}
		dirs := ff.dirs
		if ff.work {
			dirs = append(slices.Clone(dirs), session.WorkDir)
		}
		for _, dir := range dirs {
			err := store.CopyFolder(ctx, ff.from, dir)
			switch {
			case errors.Is(err, harness.ErrFileNotFound):
				s.o.Log.WarnContext(ctx, "a fork's folder names nothing in its parent's", "session", fork.ID, "dir", dir)
			case err != nil:
				return filesRefused(err, "", "copy "+dir+" of "+ff.from.Path+": "+err.Error())
			}
		}
		for i, f := range ff.files {
			if err := putFile(ctx, store, f, ff.refs[i]); err != nil {
				return err
			}
		}
		return nil
	}
}
