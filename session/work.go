// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"path"
	"slices"
	"strings"
)

// The bounds of the copy a session keeps of its working directory in its
// folder of a files store (spec 073). The copy holds files in path order
// up to its own bounds, and one turn end writes at most its own; what
// changed past them is written by a later turn end. MaxWorkFiles is half
// of the most objects a files API copies in one folder copy, so a fork's
// copy of the whole of it is one call.
const (
	// MaxWorkFileBytes is the most bytes one file of the copy holds.
	MaxWorkFileBytes = 32 << 20
	// MaxWorkFiles is the most files the copy holds.
	MaxWorkFiles = 5000
	// MaxWorkBytes is the most bytes the copy's files hold together.
	MaxWorkBytes = 256 << 20
	// MaxWorkTurnFiles is the most files one turn end writes.
	MaxWorkTurnFiles = 1000
	// MaxWorkTurnBytes is the most bytes one turn end writes.
	MaxWorkTurnBytes = 64 << 20
)

// WorkLeftOut are the directories the copy of a working directory leaves
// out at any depth, by name (spec 073): installed dependencies and caches
// a command makes again from a project's manifests, and every
// repository's history, .git, whose files are kept as plain files. A
// build's output, such as dist, build, out or .next, is kept.
var WorkLeftOut = []string{
	".git", "node_modules", "bower_components", ".venv", "venv", "__pycache__",
	".pytest_cache", ".mypy_cache", ".ruff_cache", ".tox", ".nox", ".cache",
	".npm", ".pnpm-store", ".yarn", ".gradle", ".m2", "target", ".terraform",
	".turbo", ".parcel-cache",
}

// LeftOutOfWork reports whether rel, a slash-separated path relative to
// the working directory, is inside a directory WorkLeftOut names.
func LeftOutOfWork(rel string) bool {
	dir := path.Dir(rel)
	if dir == "." {
		return false
	}
	return slices.ContainsFunc(strings.Split(dir, "/"), func(seg string) bool { return slices.Contains(WorkLeftOut, seg) })
}

// The reasons of a file the copy leaves out, and of a file a restore
// from the copy could not write.
const (
	// WorkTooLarge is a file past MaxWorkFileBytes.
	WorkTooLarge = "too_large"
	// WorkLimit is a file past the copy's files or bytes, in path order.
	WorkLimit = "limit"
	// OmittedCopy is a file the copy holds whose file the session's
	// folder no longer had at a restore, and that the log could not give
	// back.
	OmittedCopy = "copy_unavailable"
)

// The codes of a save of the copy that stopped short.
const (
	// WorkMachineUnavailable is a working directory the machine did not
	// walk.
	WorkMachineUnavailable = "machine_unavailable"
	// WorkFilesUnavailable is a write or a delete the files store did
	// not take, for another reason than the file's size.
	WorkFilesUnavailable = "files_unavailable"
)

// WorkSaved is the payload of work.saved (spec 073): what one turn end
// wrote of the working directory into the session's folder under
// WorkDir, and what it removed and left out. Covers is the last sequence
// of the log the turn end had read when it walked the directory: the
// copy holds each of its files as it was after every event up to there.
// Written are the files written, new or changed; Removed the paths the
// copy no longer holds, whose files were deleted; Unsaved the paths whose
// bytes changed and were not written, which the copy no longer vouches
// for; Deferred how many changed files the turn's bounds left for later.
// Skipped counts the files the copy leaves out as it stands and
// SkippedFiles names the first MaxRestoreNames, and Held is what the copy
// holds after the save. Failed says why a save stopped short. Whole is a
// list of every file of the copy, which replaces what earlier events
// recorded, as a fork records its parent's copy, From: Written is then
// the parent's copy as it stood, and Unsaved the paths the parent's
// folder may hold an older file at, which the fork's copy holds too.
type WorkSaved struct {
	Covers       uint64        `json:"covers"`
	Whole        bool          `json:"whole,omitempty"`
	From         string        `json:"from,omitempty"`
	Written      []WorkFile    `json:"written,omitempty"`
	Removed      []string      `json:"removed,omitempty"`
	Unsaved      []string      `json:"unsaved,omitempty"`
	Deferred     int           `json:"deferred,omitempty"`
	Skipped      int           `json:"skipped,omitempty"`
	SkippedFiles []OmittedFile `json:"skipped_files,omitempty"`
	Held         WorkHeld      `json:"held"`
	Failed       *WorkFailure  `json:"failed,omitempty"`
}

// WorkFile is one file of the copy: its path relative to the working
// directory, its size, its permission bits, and the hex SHA-256 of its
// bytes. The digest is bare hex, as a tool's result writes a file's, and
// never "sha256:<hex>": a store reads every string of that form in a
// payload as a blob of the session (Event.Blobs), and a copy's file is
// none.
type WorkFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
	SHA256 string `json:"sha256"`
}

// WorkHeld is what the copy holds: its files and their bytes together.
type WorkHeld struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// WorkFailure is why a save of the copy stopped short: its code, one of
// WorkMachineUnavailable and WorkFilesUnavailable, and a developer's
// detail.
type WorkFailure struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// WorkCopy is the copy of a working directory a session's log records
// (spec 073), the fold of its work.saved events in order.
type WorkCopy struct {
	// Files are the copy's files by path.
	Files map[string]WorkFile
	// Removed is, for each path a save removed, the Covers of the latest
	// save that removed it: a command deleted the file after every event
	// up to there.
	Removed map[string]uint64
	// Stale are the paths the folder may hold an older file at that the
	// copy does not vouch for: a later save writes over them or deletes
	// them.
	Stale map[string]bool
	// Covers is the latest save's Covers, Turn its turn and Inherited
	// whether it is a fork's copy of its parent's. Saved is false for a
	// log that records no save.
	Covers    uint64
	Turn      int
	Inherited bool
	Saved     bool
	// Pending is whether the latest save left files for a later one: it
	// deferred some, or it stopped short.
	Pending bool
	// Skipped and SkippedFiles are what the latest save that walked the
	// directory left out.
	Skipped      int
	SkippedFiles []OmittedFile
}

// Bytes are the copy's files' bytes together.
func (c WorkCopy) Bytes() int64 {
	var n int64
	for _, f := range c.Files {
		n += f.Size
	}
	return n
}

// Sorted are the copy's files in path order.
func (c WorkCopy) Sorted() []WorkFile {
	out := make([]WorkFile, 0, len(c.Files))
	for _, f := range c.Files {
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b WorkFile) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// CopyOf folds the work.saved events of log into the copy they record.
func CopyOf(log []Event) WorkCopy {
	c := WorkCopy{Files: map[string]WorkFile{}, Removed: map[string]uint64{}, Stale: map[string]bool{}}
	for _, e := range log {
		var w WorkSaved
		if e.Type != TypeWorkSaved || e.Decode(&w) != nil {
			continue
		}
		if w.Whole {
			for p := range c.Files {
				c.Stale[p] = true
			}
			c.Files, c.Removed = map[string]WorkFile{}, map[string]uint64{}
		}
		for _, f := range w.Written {
			c.Files[f.Path] = f
			delete(c.Stale, f.Path)
			delete(c.Removed, f.Path)
		}
		for _, p := range w.Removed {
			delete(c.Files, p)
			delete(c.Stale, p)
			c.Removed[p] = w.Covers
		}
		for _, p := range w.Unsaved {
			delete(c.Files, p)
			c.Stale[p] = true
		}
		c.Pending = w.Deferred > 0 || w.Failed != nil
		// A save whose walk failed saw nothing of the directory, and says
		// nothing of when the copy was last current.
		if w.Failed != nil && w.Failed.Code == WorkMachineUnavailable {
			continue
		}
		c.Saved, c.Covers, c.Turn, c.Inherited = true, w.Covers, e.Turn, w.From != ""
		c.Skipped, c.SkippedFiles = w.Skipped, w.SkippedFiles
	}
	return c
}
