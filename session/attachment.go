// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bytes"
	"mime"
	"path"
	"slices"
	"strings"
	"unicode/utf8"
)

// AttachmentDir is the directory under the working directory the files
// of a person's messages are written into (spec 015).
const AttachmentDir = "attachments"

// Attachment is one file a user.message carries (spec 015): its bytes
// are the session's blob Blob, and the runner writes them at Path,
// relative to the working directory, under AttachmentDir in a directory
// named after the message's event id. Ref, when set, is where the file
// is in the session's files store folder (spec 068), and Blob is then
// its digest alone. Source, when set, is the file of the sender's own
// files the server read the bytes from at the send, and Inline marks a
// short text file the model reads in its request, after the line that
// names it, read where the file is kept whenever a request is built and
// never kept in the log (spec 079).
type Attachment struct {
	Name      string           `json:"name"`
	MediaType string           `json:"media_type"`
	Size      int64            `json:"size"`
	Blob      Digest           `json:"blob"`
	Path      string           `json:"path"`
	Ref       string           `json:"ref,omitempty"`
	Source    AttachmentSource `json:"source,omitzero"`
	Inline    bool             `json:"inline,omitempty"`
}

// OwnerMe is the owner segment of a source that names its sender's own
// space (spec 079), the files API's alias for its caller's.
const OwnerMe = "me"

// AttachmentSource names a file of a person's own files at the files
// API a session's folder is at (spec 079): the API's owner segment,
// OwnerMe or an owner's id, and the path in that space, its plane
// included, as the message named them, and the version read, 0 for the
// file as it stood.
type AttachmentSource struct {
	Owner   string `json:"owner"`
	Path    string `json:"path"`
	Version int    `json:"version,omitempty"`
}

// The bounds of the text a model reads of a person's message's files in
// its request (spec 079): each file marked Inline is at most
// InlineTextBytes, and a message's marked files together at most
// InlineMessageTextBytes.
const (
	InlineTextBytes        = 64 << 10
	InlineMessageTextBytes = 192 << 10
)

// inlineTypes are the media types besides text/* whose file a model may
// read in its request, and inlineExtensions the name endings that mark a
// file as text whatever its media type says.
var (
	inlineTypes      = []string{"application/json", "application/xml", "application/yaml", "application/x-yaml"}
	inlineExtensions = []string{".md", ".txt", ".csv", ".tsv", ".json", ".yaml", ".yml", ".xml"}
)

// TextLike reports whether a file of the name and media type reads as
// text: a text/* type or one of inlineTypes, or a name that ends with one
// of inlineExtensions, case aside.
func TextLike(name, media string) bool {
	if base, _, err := mime.ParseMediaType(media); err == nil && (strings.HasPrefix(base, "text/") || slices.Contains(inlineTypes, base)) {
		return true
	}
	return slices.Contains(inlineExtensions, strings.ToLower(path.Ext(name)))
}

// Inlinable reports whether a file of the name, media type and bytes may
// be marked Inline on its own: text-like, UTF-8 without a NUL, and at
// most InlineTextBytes. The bound of a message's files together is the
// caller's to keep.
func Inlinable(name, media string, data []byte) bool {
	return len(data) <= InlineTextBytes && TextLike(name, media) && utf8.Valid(data) && bytes.IndexByte(data, 0) < 0
}

// AttachmentsDelivered is the payload of attachments.delivered: the
// paths the runner wrote into the machine the latest session.machine
// records.
type AttachmentsDelivered struct {
	Paths []string `json:"paths"`
}

// Attachments are the files of every message of the log, in order.
func Attachments(log []Event) []Attachment {
	var out []Attachment
	for _, e := range log {
		if e.Type != TypeUserMessage || e.Redacted() {
			continue
		}
		var p UserMessage
		if e.Decode(&p) == nil {
			out = append(out, p.Attachments...)
		}
	}
	return out
}

// PendingAttachments are the files of the log's messages that the
// session's latest machine has not been given: every one since that
// machine's session.machine that no attachments.delivered after it
// names. A session with no machine recorded has every file pending. A
// waiting message nothing read yet, or one withdrawn, has none pending.
func PendingAttachments(log []Event) []Attachment {
	delivered := map[string]bool{}
	for _, e := range log {
		switch {
		case e.Redacted():
		case e.Type == TypeSessionMachine:
			clear(delivered)
		case e.Type == TypeAttachmentsDelivered:
			var p AttachmentsDelivered
			if e.Decode(&p) == nil {
				for _, path := range p.Paths {
					delivered[path] = true
				}
			}
		}
	}
	// A waiting message's files wait with it: they are written once a
	// request reads the message, and never for a withdrawn one (spec 098).
	held := map[string]bool{}
	for _, w := range WaitingMessages(log) {
		if w.State != MessageRead {
			held[w.Event.ID] = true
		}
	}
	var out []Attachment
	for _, e := range log {
		if e.Type != TypeUserMessage || e.Redacted() || held[e.ID] {
			continue
		}
		var p UserMessage
		if e.Decode(&p) != nil {
			continue
		}
		for _, a := range p.Attachments {
			if !delivered[a.Path] && !slices.ContainsFunc(out, func(b Attachment) bool { return b.Path == a.Path }) {
				out = append(out, a)
			}
		}
	}
	return out
}

// AttachmentPath is the path the file named name of the message whose
// event id is message is written at: under AttachmentDir, in a directory
// of the message's own, so no two messages write one path.
func AttachmentPath(message, name string) string {
	return path.Join(AttachmentDir, message, name)
}
