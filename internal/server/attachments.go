// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"latere.ai/x/pkg/bearer"
	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/internal/filestore"
	"latere.ai/x/topos/session"
)

// The limits of what a person's message carries (spec 015). Each image
// and each file is counted decoded; the event body, MaxEventBody, bounds
// them together in their base64 form. An image is at most what the read
// tool gives a model, the largest a provider takes.
const (
	MaxImages          = tools.MaxImages
	MaxImageBytes      = tools.ReadMaxImage
	MaxAttachments     = 8
	MaxAttachmentBytes = 5 << 20
	// MaxAttachmentName bounds a file's name in bytes, a file system's
	// usual limit on one path segment, and a source's owner.
	MaxAttachmentName = 255
	// MaxSourceBytes bounds a file a message names by its source (spec
	// 079): the files API answers a read in one response up to its
	// inline bound, and redirects past it, which the core does not follow.
	MaxSourceBytes = filestore.InlineBytes
	// MaxSourcePath bounds a source's path in bytes.
	MaxSourcePath = 1024
)

// CodeAttachmentTooLarge is an image or a file past its limit.
const CodeAttachmentTooLarge = "attachment_too_large"

// CodeAttachmentSourceNotFound is a file a message names by its source
// that the files API answered 401, 403 or 404 for, one code for the
// three, so the answer does not say whether the file exists (spec 079).
const CodeAttachmentSourceNotFound = "attachment_source_not_found"

// SourceRule is the send and fork routes' sentence on a file a message
// names by its source (spec 079).
var SourceRule = fmt.Sprintf("A file may instead be {\"name\", \"source\": {\"owner\", \"path\", \"version\"}}, a file of the sender's own files at the files API of the session's folder of a files store: "+
	"owner the files API's owner segment, me or an owner's id; path the file's path in that space, its plane included, as GET {files API}/files/{owner}/{path} reads it; and version, optional, a version of the file from 1 upward, read as ?version=N. "+
	"A file names exactly one of data, blob and source, and a source names no media_type; an owner that is empty or past %d bytes, a path that is empty, past %d bytes or not clean, either holding a control or a bidirectional character, and a version below 1 are invalid_request. "+
	"The server reads a source once the authorizer allows the send and before the message is appended, at the files API the allow named for the session's folder, with the request's own bearer and no other credential, following no redirect: "+
	"an answer of 401, 403 or 404 is %s, one answer for the three; a redirect, which the files API gives a file past its inline bound and a public file, and a file past %d bytes are %s; and any other failure is %s. "+
	"A session that keeps no folder refuses a source as invalid_request. The file's media type is the files API's Content-Type, or its bytes' where that is empty or application/octet-stream; "+
	"it is then kept as a data file is, and its record names source as the message named it. A source counts among the message's files and its attachments.",
	MaxAttachmentName, MaxSourcePath, CodeAttachmentSourceNotFound, MaxSourceBytes, CodeAttachmentTooLarge, CodeFilesUnavailable)

// InlineRule is the send and fork routes' sentence on a text file the
// model reads in its request (spec 079).
var InlineRule = fmt.Sprintf("A file of a text/* type, application/json, application/xml, application/yaml or application/x-yaml, or whose name ends with .md, .txt, .csv, .tsv, .json, .yaml, .yml or .xml, "+
	"that is UTF-8 without a NUL and at most %d bytes is recorded inline: true, in the message's order while its inline files stay within %d bytes together, a file kept by its blob keeping its own record's mark: "+
	"the model reads its text in its request, after the line that names the file, read from where the file is kept whenever a request is built and never kept in the log.",
	session.InlineTextBytes, session.InlineMessageTextBytes)

// MaxAnswers bounds a message's answers in bytes (spec 063).
const MaxAnswers = 512

// messageBody is a user.message as a client sends it: content blocks,
// text and inline images, and the files it attaches as base64 data,
// which the server stores as blobs of the session. A sender in the body
// is read and ignored, since the verified subject is the sender.
//
// Askable and Answers are the message's word to the authorizer (spec
// 063): Askable that the client puts an authorizer's ask on the message
// to the person, and Answers the ask, as a refusal of an earlier copy of
// the message named it, that the message is sent in answer to. The core
// reads nothing into either: each is forwarded on the question about the
// message and kept in no event.
type messageBody struct {
	Sender      json.RawMessage  `json:"sender,omitempty"`
	Content     []lux.Block      `json:"content"`
	Attachments []attachmentBody `json:"attachments,omitempty"`
	Askable     bool             `json:"askable,omitempty"`
	Answers     *string          `json:"answers,omitempty"`
}

// attachmentBody is one file as a client sends it: its bytes as base64
// data; or, in a fork's message alone, the blob of a file a message of
// the session forked attached, which the fork keeps without the bytes
// being sent again (spec 056); or the source of a file of the sender's
// own files, which the server reads at the send (spec 079).
type attachmentBody struct {
	Name      string         `json:"name"`
	MediaType string         `json:"media_type,omitempty"`
	Data      string         `json:"data,omitempty"`
	Blob      session.Digest `json:"blob,omitempty"`
	Source    *sourceBody    `json:"source,omitempty"`
}

// sourceBody is a file of the sender's own files as a message names it:
// the files API's owner segment and the path in that space, and the
// version, absent for the file as it stands.
type sourceBody struct {
	Owner   string `json:"owner"`
	Path    string `json:"path"`
	Version *int   `json:"version,omitempty"`
}

// file is one attached file checked and decoded, before it is stored:
// its bytes; the blob a message of the session forked names, whose
// media type, size, ref, source and mark are that message's record of
// it; or the source its bytes are read from once the send is allowed.
// inline is the file's mark (spec 079), set by markInline.
type file struct {
	name, media string
	data        []byte
	blob        session.Digest
	size        int64
	ref         string
	source      *session.AttachmentSource
	inline      bool
}

// record is the file's record as its message keeps it, at path, with
// ref set where the file is in the session's folder.
func (f file) record(path, ref string) session.Attachment {
	a := session.Attachment{Name: f.name, MediaType: f.media, Size: int64(len(f.data)), Blob: session.DigestOf(f.data), Path: path, Ref: ref, Inline: f.inline}
	if f.source != nil {
		a.Source = *f.source
	}
	return a
}

// check reads a person's user.message: content or attachments, the
// content checked by checkContent and the files by checkAttachments,
// blobs naming the files a message of the session forked attached where
// a fork's message is read. It answers the message's payload from sender
// without its files, which are stored once the session is known to take
// it, and the files.
func (m messageBody) check(sender session.Sender, blobs bool) (session.UserMessage, []file, error) {
	if len(m.Content) == 0 && len(m.Attachments) == 0 {
		return session.UserMessage{}, nil, refuse(CodeInvalidRequest, "a user.message holds content or attachments")
	}
	if err := checkContent(m.Content); err != nil {
		return session.UserMessage{}, nil, err
	}
	files, err := checkAttachments(m.Attachments, blobs)
	if err != nil {
		return session.UserMessage{}, nil, err
	}
	if err := checkAnswers(m.Answers); err != nil {
		return session.UserMessage{}, nil, err
	}
	if m.Content == nil {
		m.Content = []lux.Block{}
	}
	return session.UserMessage{Sender: sender, Content: m.Content}, files, nil
}

// checkAnswers refuses an answers that is present and names no ask: one
// that is empty, longer than MaxAnswers bytes, or has space around it.
func checkAnswers(a *string) error {
	switch {
	case a == nil:
		return nil
	case *a == "":
		return refuse(CodeInvalidRequest, "answers is empty; it names the ask the message answers, or is left out")
	case len(*a) > MaxAnswers:
		return refuse(CodeInvalidRequest, "answers is %d bytes, more than %d", len(*a), MaxAnswers)
	case strings.TrimSpace(*a) != *a:
		return refuse(CodeInvalidRequest, "answers %q has space around it", *a)
	}
	return nil
}

// word is the message's word to the authorizer as the question about the
// message carries it: askable only when true, and answers when present.
func (m messageBody) word() map[string]any {
	out := map[string]any{}
	if m.Askable {
		out["askable"] = true
	}
	if m.Answers != nil {
		out["answers"] = *m.Answers
	}
	return out
}

// checkContent refuses content that is not text and inline images: an
// image is base64 data of PNG, JPEG, GIF or WebP whose bytes are the
// format its media type names, at most MaxImageBytes, and a message holds
// at most MaxImages of them. An image by URL is refused, since its size
// cannot be held to the limit.
func checkContent(blocks []lux.Block) error {
	images := 0
	for i, b := range blocks {
		switch b.Type {
		case ir.BlockText:
			if b.Image != nil || b.ToolUse != nil || b.ToolResult != nil || b.Opaque != nil {
				return refuse(CodeInvalidRequest, "content[%d] is a text block with fields of another type", i)
			}
		case ir.BlockImage:
			images++
			if images > MaxImages {
				return refuse(CodeInvalidRequest, "a user.message holds at most %d images", MaxImages)
			}
			img := b.Image
			switch {
			case img == nil || img.Data == "":
				return refuse(CodeInvalidRequest, "content[%d] is an image without inline data", i)
			case img.URL != "":
				return refuse(CodeInvalidRequest, "content[%d] is an image by URL; an image is sent inline, as base64 data", i)
			}
			if limit := base64.StdEncoding.EncodedLen(MaxImageBytes); len(img.Data) > limit {
				return refuse(CodeAttachmentTooLarge, "content[%d] is an image past %d bytes", i, MaxImageBytes)
			}
			data, err := base64.StdEncoding.DecodeString(img.Data)
			if err != nil {
				return refuse(CodeInvalidRequest, "content[%d]: the image's data is not base64", i)
			}
			if len(data) > MaxImageBytes {
				return refuse(CodeAttachmentTooLarge, "content[%d] is an image of %d bytes, at most %d", i, len(data), MaxImageBytes)
			}
			if media := tools.ImageType(data); media == "" || media != img.MediaType {
				return refuse(CodeInvalidRequest, "content[%d]: an image is PNG, JPEG, GIF or WebP whose media_type names its format; the data is %q and media_type says %q", i, media, img.MediaType)
			}
		default:
			return refuse(CodeInvalidRequest, "content[%d] is of type %q; a user.message holds text and image blocks", i, b.Type)
		}
	}
	return nil
}

// checkAttachments decodes a message's files: at most MaxAttachments,
// each a name that is one path segment of at most MaxAttachmentName
// bytes without a control or a bidirectional character, no two of one
// name, base64 data of
// at most MaxAttachmentBytes, and a media type, the one given or the
// one the bytes give. Where blobs is set, as in a fork's message, a file
// may name a blob in place of its data and no media type, which the
// fork reads from the message that attached it. A file may name a source
// in place of its data and no media type, which the server reads once
// the send is allowed (spec 079). A file names exactly one of data, a
// blob and a source.
func checkAttachments(in []attachmentBody, blobs bool) ([]file, error) {
	if len(in) > MaxAttachments {
		return nil, refuse(CodeInvalidRequest, "a user.message attaches at most %d files", MaxAttachments)
	}
	out := make([]file, 0, len(in))
	names := map[string]bool{}
	for i, a := range in {
		if err := checkName(a.Name); err != nil {
			return nil, refuse(CodeInvalidRequest, "attachments[%d]: %v", i, err)
		}
		if names[a.Name] {
			return nil, refuse(CodeInvalidRequest, "attachments[%d]: the message attaches another file named %q", i, a.Name)
		}
		names[a.Name] = true
		switch {
		case a.Source != nil && (a.Data != "" || a.Blob != ""):
			return nil, refuse(CodeInvalidRequest, "attachments[%d] names a source beside data or a blob; a file is one of the three", i)
		case a.Source != nil && a.MediaType != "":
			return nil, refuse(CodeInvalidRequest, "attachments[%d] names a source and a media_type; a source's media type is the one the files API answers", i)
		case a.Source != nil:
			src, err := a.Source.check()
			if err != nil {
				return nil, refuse(CodeInvalidRequest, "attachments[%d]: %v", i, err)
			}
			out = append(out, file{name: a.Name, source: &src})
			continue
		case a.Blob != "" && !blobs:
			return nil, refuse(CodeInvalidRequest, "attachments[%d] names a blob; a message sent to a session attaches its file's data, and only a fork's message keeps a file of the session it forks by its blob", i)
		case a.Blob != "" && a.Data != "":
			return nil, refuse(CodeInvalidRequest, "attachments[%d] names both data and a blob; a file is one of the two", i)
		case a.Blob != "" && a.MediaType != "":
			return nil, refuse(CodeInvalidRequest, "attachments[%d] names a blob and a media_type; a kept file's media type is the one its message recorded", i)
		case a.Blob != "" && !a.Blob.Valid():
			return nil, refuse(CodeInvalidRequest, "attachments[%d]: the blob %q is not a sha256: digest", i, a.Blob)
		case a.Blob != "":
			out = append(out, file{name: a.Name, blob: a.Blob})
			continue
		case a.Data == "" && blobs:
			return nil, refuse(CodeInvalidRequest, "attachments[%d] names none of data, a blob and a source; a file is one of the three", i)
		}
		if limit := base64.StdEncoding.EncodedLen(MaxAttachmentBytes); len(a.Data) > limit {
			return nil, refuse(CodeAttachmentTooLarge, "attachments[%d] is past %d bytes", i, MaxAttachmentBytes)
		}
		data, err := base64.StdEncoding.DecodeString(a.Data)
		if err != nil {
			return nil, refuse(CodeInvalidRequest, "attachments[%d]: the data is not base64", i)
		}
		if len(data) > MaxAttachmentBytes {
			return nil, refuse(CodeAttachmentTooLarge, "attachments[%d] is %d bytes, at most %d", i, len(data), MaxAttachmentBytes)
		}
		media := a.MediaType
		if media == "" {
			media = http.DetectContentType(data)
		} else if _, _, err := mime.ParseMediaType(media); err != nil {
			return nil, refuse(CodeInvalidRequest, "attachments[%d]: media_type %q is not a media type", i, media)
		}
		out = append(out, file{name: a.Name, media: media, data: data})
	}
	return out, nil
}

// check reads a source as a message names it: an owner of at most
// MaxAttachmentName bytes and a path of at most MaxSourcePath, each
// UTF-8 without a control or a bidirectional character, the path
// relative and clean, and a version from 1 upward when it names one.
func (b *sourceBody) check() (session.AttachmentSource, error) {
	switch {
	case b.Owner == "":
		return session.AttachmentSource{}, errors.New("the source names no owner")
	case len(b.Owner) > MaxAttachmentName:
		return session.AttachmentSource{}, fmt.Errorf("the source's owner is %d bytes, at most %d", len(b.Owner), MaxAttachmentName)
	case b.Path == "":
		return session.AttachmentSource{}, errors.New("the source names no path")
	case len(b.Path) > MaxSourcePath:
		return session.AttachmentSource{}, fmt.Errorf("the source's path is %d bytes, at most %d", len(b.Path), MaxSourcePath)
	case b.Version != nil && *b.Version < 1:
		return session.AttachmentSource{}, fmt.Errorf("the source's version is %d; a version counts from 1", *b.Version)
	}
	if err := plainText(b.Owner); err != nil {
		return session.AttachmentSource{}, fmt.Errorf("the source's owner %w", err)
	}
	if err := plainText(b.Path); err != nil {
		return session.AttachmentSource{}, fmt.Errorf("the source's path %w", err)
	}
	for seg := range strings.SplitSeq(b.Path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return session.AttachmentSource{}, fmt.Errorf("the source's path %q is not clean: it begins or ends with /, or holds an empty, . or .. segment", b.Path)
		}
	}
	src := session.AttachmentSource{Owner: b.Owner, Path: b.Path}
	if b.Version != nil {
		src.Version = *b.Version
	}
	return src, nil
}

// plainText refuses text that is not UTF-8 or that holds a control or a
// bidirectional character.
func plainText(s string) error {
	if !utf8.ValidString(s) {
		return errors.New("is not UTF-8")
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return errors.New("holds a control or a bidirectional character")
		}
	}
	return nil
}

// checkName refuses a file name that is not one plain path segment.
func checkName(name string) error {
	switch {
	case name == "" || name == "." || name == "..":
		return fmt.Errorf("the name %q is not a file's", name)
	case len(name) > MaxAttachmentName:
		return fmt.Errorf("the name is %d bytes, at most %d", len(name), MaxAttachmentName)
	case !utf8.ValidString(name):
		return fmt.Errorf("the name is not UTF-8")
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("the name %q holds a path separator; a file is named by one path segment", name)
	}
	if err := plainText(name); err != nil {
		return fmt.Errorf("the name %w", err)
	}
	return nil
}

// storeAttachments stores each file as a blob of the session and names
// the path it is written at, in the directory of the message whose event
// id is message. A session with a folder of the files store writes each
// there instead, at that same path, and its record names it by ref (spec
// 068). Each record carries the file's source and its mark (spec 079).
func (c *call) storeAttachments(ctx context.Context, s session.Session, message string, files []file) ([]session.Attachment, error) {
	markInline(files)
	out := make([]session.Attachment, 0, len(files))
	if s.Folder != nil {
		for _, f := range files {
			p := session.AttachmentPath(message, f.name)
			out = append(out, f.record(p, p))
		}
		if err := c.s.putAttachments(ctx, s, files, out); err != nil {
			return nil, err
		}
		return out, nil
	}
	for _, f := range files {
		d, err := c.s.o.Sessions.PutBlob(ctx, s.ID, bytes.NewReader(f.data))
		if err != nil {
			return nil, err
		}
		a := f.record(session.AttachmentPath(message, f.name), "")
		a.Blob = d
		out = append(out, a)
	}
	return out, nil
}

// markInline marks the files of one message whose text the model reads
// in its request (spec 079), in the message's order: a file of bytes
// session.Inlinable holds to, and a file kept by its blob that its own
// message marked, each while the marked files together stay within
// session.InlineMessageTextBytes. A file that would pass the bound is not
// marked, and a later smaller one may be.
func markInline(files []file) {
	var total int64
	for i := range files {
		f := &files[i]
		size, ok := int64(len(f.data)), session.Inlinable(f.name, f.media, f.data)
		if f.blob != "" {
			size, ok = f.size, f.inline
		}
		f.inline = ok && total+size <= session.InlineMessageTextBytes
		if f.inline {
			total += size
		}
	}
}

// readSources reads each file of a message that names a source (spec
// 079), once the send is allowed and before anything is written: at the
// files API of sess's folder, the address the allow named, with the
// bearer r was authenticated with, the sender's own, and no other
// credential. A session without a folder refuses a source, since the
// file is read where the folder is and kept there. A source the files
// API refuses with 401, 403 or 404 is attachment_source_not_found, one
// it redirects or that is past MaxSourceBytes attachment_too_large, and
// any other failure files_unavailable. The media type is the files API's,
// or the one the bytes give where it names none, or names
// application/octet-stream.
func (s *Server) readSources(ctx context.Context, sess session.Session, r *http.Request, files []file) error {
	first := -1
	for i, f := range files {
		if f.source != nil {
			first = i
			break
		}
	}
	if first < 0 {
		return nil
	}
	switch {
	case sess.Folder == nil:
		return refuse(CodeInvalidRequest, "attachments[%d] names a source; a file of a person's files is read at the files API of the session's folder and kept there, and this session keeps none", first)
	case s.o.Files == nil || s.o.Files.Source == nil:
		return &apiError{code: CodeFilesUnavailable, detail: "this server reads no file of a person's files: its files store has no Source"}
	}
	token, ok := bearer.FromRequest(r)
	if !ok {
		return refuse(CodeInvalidRequest, "the request carries no bearer in its Authorization header to read attachments[%d]'s source with", first)
	}
	for i := range files {
		f := &files[i]
		if f.source == nil {
			continue
		}
		data, ctype, err := s.o.Files.Source(ctx, sess.Folder.URL, token, *f.source, MaxSourceBytes)
		switch {
		case errors.Is(err, filestore.ErrSourceNotFound):
			// The detail is one for the three answers, as the code is.
			return refuse(CodeAttachmentSourceNotFound, "attachments[%d]: the files API holds no file at %s of %s that the sender may read", i, f.source.Path, f.source.Owner)
		case errors.Is(err, filestore.ErrSourceTooLarge):
			return &apiError{code: CodeAttachmentTooLarge, detail: fmt.Sprintf("attachments[%d]: %v; a source is at most %d bytes", i, err, MaxSourceBytes), err: err}
		case err != nil:
			return filesRefused(err, "", fmt.Sprintf("read attachments[%d]'s source from the files API: %v", i, err))
		}
		f.data, f.media = data, sourceMedia(ctype, data)
	}
	return nil
}

// sourceMedia is a source's media type: the files API's Content-Type,
// unless it is empty, not a media type or application/octet-stream,
// which say nothing of the file, and then the one its bytes give, as a
// data file's without a media type.
func sourceMedia(ctype string, data []byte) string {
	if base, _, err := mime.ParseMediaType(ctype); err == nil && base != "application/octet-stream" {
		return ctype
	}
	return http.DetectContentType(data)
}

// attached is each file the user.messages of a log attach, by its blob:
// the record of the first message that attached it.
func attached(evs []session.Event) map[session.Digest]session.Attachment {
	out := map[session.Digest]session.Attachment{}
	for _, e := range evs {
		var m session.UserMessage
		if e.Type != session.TypeUserMessage || e.Redacted() || e.Decode(&m) != nil {
			continue
		}
		for _, a := range m.Attachments {
			if _, ok := out[a.Blob]; !ok {
				out[a.Blob] = a
			}
		}
	}
	return out
}
