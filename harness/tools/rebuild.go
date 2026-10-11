// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"path"
	"slices"
	"strconv"
	"strings"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/session"
)

// The bounds of a working directory rebuilt from a session's log (spec
// 065): enough for the files an agent writes in a long session many
// times over, and too little for a log to fill a machine's disk with.
const (
	// MaxRebuiltFiles is the most files a rebuild holds.
	MaxRebuiltFiles = 2000
	// MaxRebuiltBytes is the most bytes the files of a rebuild hold
	// together.
	MaxRebuiltBytes = 64 << 20
)

// RebuiltFile is one file of a working directory as a session's log
// holds it. Its bytes are Content, or the blob Blob names for a file a
// files.kept stored.
type RebuiltFile struct {
	// Path is the file's path under the working directory, relative and
	// slash-separated.
	Path    string
	Content []byte
	Blob    session.Digest
	Size    int64
	// Unverified is true when the log recorded a hash for the file after
	// its last write, edit or read that the rebuilt bytes do not have:
	// something the log does not hold, a command, changed the file since.
	Unverified bool
}

// Rebuilt is a working directory as a session's log holds it: Files, by
// path, the files it can give back; Omitted, by path, the files the log
// shows were there and that it cannot give back, each with why; and
// Commands, how many commands ran since the first of those files was
// made, whose own files the log never held.
type Rebuilt struct {
	Files    []RebuiltFile
	Omitted  []session.OmittedFile
	Commands int
}

// rebuilding is one path while a log is replayed: a file with its bytes,
// or one that is omitted for a reason.
type rebuilding struct {
	file    RebuiltFile
	omitted string
}

// Rebuild replays a session's log into the working directory it holds
// (spec 065). In the log's order: a successful write gives a file its
// content; a successful edit is applied to the file as rebuilt so far; a
// read of a whole file whose text, or image, hashes to the hash its
// result recorded gives the file those bytes, which is how a file a
// command made comes back; and a files.kept gives each file it stored
// the bytes of its blob. A path is under the working directory the
// latest session.machine names, and one outside it is not a file of the
// rebuild.
//
// A file the log shows and cannot give back is omitted with a reason: an
// edit or a partial read of a file whose content no event holds, an edit
// whose text is not in the file as rebuilt, a file past MaxRebuiltFiles
// or MaxRebuiltBytes. A later write, full read or kept blob gives it
// back. A hash a later result recorded that the rebuilt bytes do not
// have marks the file unverified. A session.machine that says the
// machine was replaced with nothing restored empties the rebuild: what
// was there before was lost then. A redacted event gives nothing, and a
// file whose latest write or edit was redacted, the call or its result,
// is not rebuilt from an older one. blob reads a kept file's bytes for an edit that follows it;
// nil omits such a file.
func Rebuild(evs []session.Event, blob func(session.Digest) ([]byte, error)) Rebuilt {
	sorted := slices.Clone(evs)
	slices.SortStableFunc(sorted, func(a, b session.Event) int { return cmp.Compare(a.Seq, b.Seq) })
	r := &replay{paths: map[string]*rebuilding{}, calls: map[string]session.AgentToolUse{}, blob: blob}
	for _, e := range sorted {
		r.event(e)
	}
	var out Rebuilt
	for p, f := range r.paths {
		if f.omitted != "" {
			out.Omitted = append(out.Omitted, session.OmittedFile{Path: p, Reason: f.omitted})
			continue
		}
		out.Files = append(out.Files, f.file)
	}
	slices.SortFunc(out.Files, func(a, b RebuiltFile) int { return strings.Compare(a.Path, b.Path) })
	slices.SortFunc(out.Omitted, func(a, b session.OmittedFile) int { return strings.Compare(a.Path, b.Path) })
	out.Commands = r.commands
	return out
}

// replay is the state of a Rebuild.
type replay struct {
	workdir string
	paths   map[string]*rebuilding
	calls   map[string]session.AgentToolUse
	// count and bytes are the files the rebuild holds bytes of and their
	// sizes together.
	count    int
	bytes    int64
	commands int
	blob     func(session.Digest) ([]byte, error)
}

// event replays one event.
func (r *replay) event(e session.Event) {
	if e.Redacted() {
		// A redacted result keeps the id of its call and nothing of what
		// the call did: a file it may have written is no longer known.
		var kept struct {
			ToolUseID string `json:"tool_use_id"`
		}
		if e.Type == session.TypeToolResult && json.Unmarshal(e.Payload, &kept) == nil {
			r.unknown(kept.ToolUseID)
		}
		return
	}
	switch e.Type {
	case session.TypeSessionMachine:
		var p session.SessionMachine
		if e.Decode(&p) != nil {
			return
		}
		r.workdir = path.Clean(p.Machine.Workdir)
		if p.Reason == session.MachineReplaced {
			r.paths, r.count, r.bytes, r.commands = map[string]*rebuilding{}, 0, 0, 0
		}
	case session.TypeAgentToolUse:
		var p session.AgentToolUse
		if e.Decode(&p) == nil {
			r.calls[p.ToolUseID] = p
		}
	case session.TypeToolResult:
		var p session.ToolResult
		if e.Decode(&p) == nil {
			r.result(p)
		}
	case session.TypeFilesKept:
		var p session.FilesKept
		if e.Decode(&p) != nil {
			return
		}
		for _, f := range p.Files {
			if rel, ok := r.rel(f.Resolved); ok {
				r.set(rel, RebuiltFile{Blob: f.Blob, Size: f.Size})
			}
		}
	}
}

// result replays one tool result with the call it answers.
func (r *replay) result(p session.ToolResult) {
	var meta Meta
	if len(p.Meta) > 0 && json.Unmarshal(p.Meta, &meta) != nil {
		return
	}
	call, ok := r.calls[p.ToolUseID]
	if !ok {
		// The call was redacted, and its result says what the file hashed
		// to after it: a file the call changed is not rebuilt, and no
		// older content stands in for it.
		if rel, in := r.rel(meta.Path); in && meta.SHA256 != "" && !r.holds(rel, meta.SHA256) {
			r.forget(rel)
		}
		return
	}
	if call.Name == NameBash {
		if len(r.paths) > 0 {
			r.commands++
		}
		return
	}
	if p.IsError || (p.Outcome != "" && p.Outcome != OutcomeOK) {
		return
	}
	rel, ok := r.rel(meta.Path)
	if !ok {
		return
	}
	switch call.Name {
	case NameWrite:
		var in writeInput
		if json.Unmarshal(call.Input, &in) != nil {
			return
		}
		r.set(rel, RebuiltFile{Content: []byte(in.Content), Unverified: differs([]byte(in.Content), meta.SHA256)})
	case NameEdit:
		var in editInput
		if json.Unmarshal(call.Input, &in) != nil {
			return
		}
		r.edit(rel, in, meta.SHA256)
	case NameRead:
		r.read(rel, p, meta.SHA256)
	}
}

// edit applies an edit to the file as rebuilt so far.
func (r *replay) edit(rel string, in editInput, want string) {
	content, ok := r.content(rel)
	if !ok {
		r.omit(rel, session.OmittedUnknown)
		return
	}
	text := string(content)
	n := strings.Count(text, in.OldString)
	if in.OldString == "" || n == 0 || (n > 1 && !in.ReplaceAll) {
		r.omit(rel, session.OmittedEdit)
		return
	}
	if in.ReplaceAll {
		text = strings.ReplaceAll(text, in.OldString, in.NewString)
	} else {
		text = strings.Replace(text, in.OldString, in.NewString, 1)
	}
	r.set(rel, RebuiltFile{Content: []byte(text), Unverified: differs([]byte(text), want)})
}

// read takes a file's bytes from a read of the whole of it, checked
// against the hash the read recorded, and otherwise holds the file as
// rebuilt to that hash.
func (r *replay) read(rel string, p session.ToolResult, want string) {
	if want == "" {
		return
	}
	if whole, ok := wholeRead(p, want); ok {
		r.set(rel, RebuiltFile{Content: whole})
		return
	}
	f, known := r.paths[rel]
	switch {
	case !known:
		// The file was there, and no event holds its content.
		r.omit(rel, session.OmittedUnknown)
	case f.omitted != "":
	case f.file.Blob != "":
		f.file.Unverified = f.file.Blob.Hex() != want
	default:
		f.file.Unverified = differs(f.file.Content, want)
	}
}

// wholeRead is the file a read's result holds in full, when its bytes
// hash to want: an empty file, an image as read returns one, or the text
// of every line behind its number from line 1, with or without a last
// newline. A read of part of a file, one whose line was cut and one cut
// at the output cap do not hash to want and are not taken.
func wholeRead(p session.ToolResult, want string) ([]byte, bool) {
	if want == digest(nil) {
		return []byte{}, true
	}
	var text strings.Builder
	for _, b := range p.Content {
		switch {
		case b.Type == ir.BlockImage && b.Image != nil:
			data, err := base64.StdEncoding.DecodeString(b.Image.Data)
			return data, err == nil && digest(data) == want
		case b.Type == ir.BlockText:
			text.WriteString(b.Text)
		}
	}
	rows := strings.Split(strings.TrimSuffix(text.String(), "\n"), "\n")
	lines := make([]string, 0, len(rows))
	for i, row := range rows {
		number, line, ok := strings.Cut(row, "\t")
		if !ok || strings.TrimSpace(number) != strconv.Itoa(i+1) {
			return nil, false
		}
		lines = append(lines, line)
	}
	joined := strings.Join(lines, "\n")
	for _, candidate := range []string{joined + "\n", joined} {
		if digest([]byte(candidate)) == want {
			return []byte(candidate), true
		}
	}
	return nil, false
}

// unknown drops the file a write or an edit named, when the call's
// result was redacted: whether it changed the file is no longer known.
func (r *replay) unknown(id string) {
	call, ok := r.calls[id]
	if !ok || (call.Name != NameWrite && call.Name != NameEdit) {
		return
	}
	var in struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(call.Input, &in) != nil {
		return
	}
	if rel, ok := r.rel(in.Path); ok {
		r.forget(rel)
	}
}

// holds reports whether the rebuild holds a file at rel whose bytes hash
// to want.
func (r *replay) holds(rel, want string) bool {
	f, ok := r.paths[rel]
	switch {
	case !ok || f.omitted != "":
		return false
	case f.file.Blob != "":
		return f.file.Blob.Hex() == want
	}
	return digest(f.file.Content) == want
}

// rel is p under the working directory, false for a path that is the
// directory itself, outside it, or met before any machine was recorded.
func (r *replay) rel(p string) (string, bool) {
	if r.workdir == "" || p == "" {
		return "", false
	}
	if !path.IsAbs(p) {
		p = path.Join(r.workdir, p)
	}
	rel, ok := strings.CutPrefix(path.Clean(p), strings.TrimSuffix(r.workdir, "/")+"/")
	return rel, ok && rel != ""
}

// content is the bytes of a file as rebuilt so far, false for a path the
// rebuild holds no bytes of.
func (r *replay) content(rel string) ([]byte, bool) {
	f, ok := r.paths[rel]
	switch {
	case !ok || f.omitted != "":
		return nil, false
	case f.file.Blob == "":
		return f.file.Content, true
	case r.blob == nil:
		return nil, false
	}
	b, err := r.blob(f.file.Blob)
	return b, err == nil
}

// set gives a path its file, or omits it when the rebuild is full.
func (r *replay) set(rel string, f RebuiltFile) {
	r.forget(rel)
	f.Path = rel
	if f.Blob == "" {
		f.Size = int64(len(f.Content))
	}
	if r.count >= MaxRebuiltFiles || r.bytes+f.Size > MaxRebuiltBytes {
		r.paths[rel] = &rebuilding{omitted: session.OmittedLimit}
		return
	}
	r.count++
	r.bytes += f.Size
	r.paths[rel] = &rebuilding{file: f}
}

// omit records a path as one the rebuild cannot give back.
func (r *replay) omit(rel, reason string) {
	r.forget(rel)
	r.paths[rel] = &rebuilding{omitted: reason}
}

// forget drops a path from the rebuild.
func (r *replay) forget(rel string) {
	if f, ok := r.paths[rel]; ok && f.omitted == "" {
		r.count--
		r.bytes -= f.file.Size
	}
	delete(r.paths, rel)
}

// differs reports whether b does not hash to want, a hash the log
// recorded; an empty want records none.
func differs(b []byte, want string) bool {
	return want != "" && digest(b) != want
}
