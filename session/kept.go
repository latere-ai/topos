// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

// FilesKept is the payload of files.kept (spec 055): the local images an
// agent.message of the session's own thread names, read from the open
// machine once the message's step was committed and stored as blobs of
// the session. Message is the agent.message's event id. Files are the
// images kept and Skipped the references that were not, each with why;
// either may be empty, and an event with both empty is never appended.
// The event renders into no prompt.
type FilesKept struct {
	Message string        `json:"message"`
	Files   []KeptFile    `json:"files"`
	Skipped []SkippedFile `json:"skipped"`
}

// KeptFile is one image files.kept stored. Path is the destination as the
// message wrote it, Resolved the absolute path read inside the working
// directory, MediaType the format its bytes are, never its name's, and
// Width and Height the size its format's header gives.
type KeptFile struct {
	Path      string `json:"path"`
	Resolved  string `json:"resolved"`
	Blob      Digest `json:"blob"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

// SkippedFile is one reference files.kept did not store, with the reason.
type SkippedFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// The reasons of a SkippedFile.
const (
	// KeptNotFound is a path nothing exists at.
	KeptNotFound = "not_found"
	// KeptTooLarge is a file past the bytes, a side or the pixels an image
	// is kept at.
	KeptTooLarge = "too_large"
	// KeptNotAnImage is a file whose bytes are none of PNG, JPEG, GIF and
	// WebP, an SVG included, or a directory.
	KeptNotAnImage = "not_an_image"
	// KeptOutsideWorkdir is a path outside the working directory, or a
	// symbolic link that leads out of it.
	KeptOutsideWorkdir = "outside_workdir"
	// KeptNoMachine is a reference of a step in a turn that opened no
	// machine: keeping an image never opens one.
	KeptNoMachine = "no_machine"
	// KeptLimit is a reference past the images one message keeps.
	KeptLimit = "limit"
	// KeptUnavailable is a read or a store that failed, which the turn
	// runs past.
	KeptUnavailable = "unavailable"
)

// Companions are the events a redaction of e takes with it, in the same
// append: the files.kept of an agent.message, since a person who removes
// an answer removes its pictures with it. A redacted companion is not
// one, and an event of any other type has none.
func Companions(e Event, log []Event) []Event {
	if e.Type != TypeAgentMessage {
		return nil
	}
	var out []Event
	for _, c := range log {
		if c.Type != TypeFilesKept || c.Redacted() {
			continue
		}
		var p FilesKept
		if c.Decode(&p) == nil && p.Message == e.ID {
			out = append(out, c)
		}
	}
	return out
}
