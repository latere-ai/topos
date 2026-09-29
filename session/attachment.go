// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"path"
	"slices"
)

// AttachmentDir is the directory under the working directory the files
// of a person's messages are written into (spec 015).
const AttachmentDir = "attachments"

// Attachment is one file a user.message carries (spec 015): its bytes
// are the session's blob Blob, and the runner writes them at Path,
// relative to the working directory, under AttachmentDir in a directory
// named after the message's event id.
type Attachment struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
	Blob      Digest `json:"blob"`
	Path      string `json:"path"`
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
// names. A session with no machine recorded has every file pending.
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
	var out []Attachment
	for _, a := range Attachments(log) {
		if !delivered[a.Path] && !slices.ContainsFunc(out, func(b Attachment) bool { return b.Path == a.Path }) {
			out = append(out, a)
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
