// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"latere.ai/x/topos/session"
)

// MaxConversationBytes bounds one file of a folder of conversations (spec
// 094): a conversation past it keeps its messages in order up to the
// bound, and the file's last line says how many were left out.
const MaxConversationBytes = 256 << 10

// Conversations is a session's reach to its folders of conversations
// (spec 094): the server's, which reads its own log and asks the
// authorizer as the session's initiator. The runners the server drives
// call it directly, and a runner process's lease asks it over the
// internal listener.
type Conversations interface {
	// ListConversations answers the files of the session's folder named
	// folder, newest first, at most MaxListed, and whether its scope held
	// more. A session the authorizer does not let the initiator read is
	// left out.
	ListConversations(ctx context.Context, folder string) (files []ConversationFile, more bool, err error)
	// ReadConversation answers the file name of the folder and its text.
	// A file that names no session of the folder's scope, or one the
	// initiator may not read, is fs.ErrNotExist.
	ReadConversation(ctx context.Context, folder, name string) (ConversationFile, []byte, error)
}

// ConversationFile is one file of a folder of conversations: its name in
// the folder, its version, which changes whenever its text does, and the
// time its session last changed. Size is the text's, known once it is
// read, and 0 in a listing.
type ConversationFile struct {
	Name     string    `json:"name"`
	Version  string    `json:"version"`
	Modified time.Time `json:"modified"`
	Size     int64     `json:"size,omitempty"`
}

// conversationDay is the layout of the day a conversation's file is named
// by.
const conversationDay = "2006-01-02"

// ConversationName is the name of s's file in a folder of conversations:
// the day it was created, in UTC, and its id, so the name holds while its
// title changes.
func ConversationName(s session.Session) string {
	return s.CreatedAt.UTC().Format(conversationDay) + "-" + s.ID + ".md"
}

// ConversationID is the session id a file's name names, false for a name
// of no other shape than ConversationName's. The caller still holds the
// session to the name, since the day is the session's own.
func ConversationID(name string) (string, bool) {
	rest, ok := strings.CutSuffix(name, ".md")
	if !ok || len(rest) <= len(conversationDay)+1 || rest[len(conversationDay)] != '-' {
		return "", false
	}
	if _, err := time.Parse(conversationDay, rest[:len(conversationDay)]); err != nil {
		return "", false
	}
	id := rest[len(conversationDay)+1:]
	if strings.ContainsAny(id, "/\\") || strings.ContainsFunc(id, func(r rune) bool { return r < ' ' }) {
		return "", false
	}
	return id, true
}

// ConversationFileOf is s's file in a listing: its name, its version and
// when it last changed.
func ConversationFileOf(s session.Session) ConversationFile {
	return ConversationFile{Name: ConversationName(s), Version: conversationVersion(s), Modified: s.UpdatedAt.UTC()}
}

// conversationVersion changes whenever a session's file would: with its
// log, its title, or the time it last changed.
func conversationVersion(s session.Session) string {
	h := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%s\x00%s", s.ID, s.LastSeq, s.Title, s.UpdatedAt.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(h[:16])
}

// conversationTime is how a file of conversations writes a time.
const conversationTime = "2006-01-02 15:04 UTC"

// RenderConversation is s's file in a folder of conversations (spec 094),
// from its log events in order: a heading of its title, a line of its id
// and dates, and a section for each event session.SaidText reads that
// says something, headed by who said it and when. Past
// MaxConversationBytes the file stops at a section's end and its last
// line says how many messages were left out; a first message past the
// bound alone is cut on a character's start.
func RenderConversation(s session.Session, events []session.Event) []byte {
	var b strings.Builder
	title := strings.TrimSpace(s.Title)
	if title == "" {
		title = "Session " + s.ID
	}
	fmt.Fprintf(&b, "# %s\n\nSession %s, started %s, last changed %s.\n", oneLine(title), s.ID,
		s.CreatedAt.UTC().Format(conversationTime), s.UpdatedAt.UTC().Format(conversationTime))
	var sections []string
	for _, e := range events {
		said, ok := session.SaidText(e)
		said = strings.TrimSpace(said)
		if !ok || said == "" {
			continue
		}
		who := "Agent"
		if e.Type != session.TypeAgentMessage {
			who = "User"
		}
		sections = append(sections, fmt.Sprintf("\n## %s, %s\n\n%s\n", who, e.Time.UTC().Format(conversationTime), said))
	}
	for i, sec := range sections {
		last := i == len(sections)-1
		if b.Len()+len(sec) <= MaxConversationBytes && (last || b.Len()+len(sec)+noteRoom <= MaxConversationBytes) {
			b.WriteString(sec)
			continue
		}
		left := len(sections) - i
		if i > 0 {
			fmt.Fprintf(&b, "\n[%d more %s of this conversation %s not in this file, which holds at most %d KiB.]\n", left, plural(left), are(left), MaxConversationBytes>>10)
			break
		}
		// The first message alone is past the bound: as much of it as
		// fits, and what is left out.
		b.WriteString(cutRunes(sec, MaxConversationBytes-noteRoom-b.Len()))
		if left == 1 {
			fmt.Fprintf(&b, "\n[The rest of this message is not in this file, which holds at most %d KiB.]\n", MaxConversationBytes>>10)
		} else {
			fmt.Fprintf(&b, "\n[The rest of this message and %d more %s of this conversation are not in this file, which holds at most %d KiB.]\n", left-1, plural(left-1), MaxConversationBytes>>10)
		}
		break
	}
	return []byte(b.String())
}

// noteRoom is the room a file keeps for the line that says what was left
// out, more than the longest such line.
const noteRoom = 160

// are is the verb a note counts messages with.
func are(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// plural is the noun a note counts messages with.
func plural(n int) string {
	if n == 1 {
		return "message"
	}
	return "messages"
}

// oneLine is a title on one line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// cutRunes is s cut to at most n bytes on a character's start.
func cutRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
