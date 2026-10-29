// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// MaxMemoryFolders is the most memory folders one session reaches (spec
// 093).
const MaxMemoryFolders = 4

// The bounds of a memory folder's parts (spec 093).
const (
	// MaxMemoryOwner is the most bytes of a memory folder's space.
	MaxMemoryOwner = 512
	// MaxMemoryPath is the most bytes of a memory folder's path in its
	// space.
	MaxMemoryPath = 512
)

// The accesses of a memory folder (spec 093).
const (
	MemoryReadOnly  = "read_only"
	MemoryReadWrite = "read_write"
)

// MemoryFolder is one memory folder an allow names for a session (spec
// 093): a folder of an Arca-compatible files API that the session's file
// tools reach at /memory/<name>/, read only or read and write, or, with
// Sessions, a read-only folder of the initiator's other conversations the
// server serves from its log (spec 094). The core reads nothing into its
// URL, owner or path; the API answers the folder by its name and access
// alone.
type MemoryFolder struct {
	// Name is the folder's directory under /memory: a lowercase letter
	// and at most 31 more lowercase letters, digits or "-", unique in the
	// session's list.
	Name string `json:"name"`
	// URL is the files API's root, under which files/{owner}/{path} is
	// served, as Folder's URL.
	URL string `json:"url,omitempty"`
	// Owner is the space the folder is in.
	Owner string `json:"owner,omitempty"`
	// Path is the folder's path in its space.
	Path string `json:"path,omitempty"`
	// Access is MemoryReadOnly or MemoryReadWrite.
	Access string `json:"access"`
	// Sessions, when set, makes the folder one of conversations (spec
	// 094) in place of a folder of a files store: URL, Owner and Path are
	// then empty and Access is MemoryReadOnly.
	Sessions *MemorySessions `json:"sessions,omitempty"`
}

// MemorySessions is which of the initiator's sessions a folder of
// conversations holds (spec 094): those its metadata entry keeps, every
// one of the folder's scope when it names none.
type MemorySessions struct {
	// Metadata is at most MaxMemorySessionsMetadata entries: a value keeps
	// the sessions whose metadata holds the key with exactly that value,
	// nil the sessions whose metadata does not hold the key.
	Metadata map[string]*string `json:"metadata,omitempty"`
}

// MaxMemorySessionsMetadata is the most metadata entries a folder of
// conversations is kept by (spec 094).
const MaxMemorySessionsMetadata = 1

// Conversations reports whether the folder is one of conversations.
func (f MemoryFolder) Conversations() bool { return f.Sessions != nil }

// Entry is the list filter of a folder of conversations: its one
// metadata entry, nil for a folder that names none or holds no
// conversations.
func (s *MemorySessions) Entry() *MetadataEntry {
	if s == nil {
		return nil
	}
	for k, v := range s.Metadata {
		if v == nil {
			return &MetadataEntry{Key: k, Absent: true}
		}
		return &MetadataEntry{Key: k, Value: *v}
	}
	return nil
}

// Writable reports whether the session may change the folder's files.
func (f MemoryFolder) Writable() bool { return f.Access == MemoryReadWrite }

// memoryName is a memory folder's name.
var memoryName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// CheckMemory reports why fs cannot be a session's memory folders (spec
// 093): at most MaxMemoryFolders, each with a name of the rule, no name
// twice, an absolute http or https URL with no user, query or fragment,
// an owner of 1 to MaxMemoryOwner bytes, a relative path of at most
// MaxMemoryPath bytes with no empty, "." or ".." segment, and an access of
// read_only or read_write. No part holds a control character.
func CheckMemory(fs []MemoryFolder) error {
	if len(fs) > MaxMemoryFolders {
		return fmt.Errorf("memory names %d folders, more than %d", len(fs), MaxMemoryFolders)
	}
	names := map[string]bool{}
	for i, f := range fs {
		if err := checkMemoryFolder(f); err != nil {
			return fmt.Errorf("memory[%d]: %w", i, err)
		}
		if names[f.Name] {
			return fmt.Errorf("memory[%d] names the folder %s a second time", i, f.Name)
		}
		names[f.Name] = true
	}
	return nil
}

func checkMemoryFolder(f MemoryFolder) error {
	if !memoryName.MatchString(f.Name) {
		return fmt.Errorf("the name %q is not a lowercase letter and at most 31 more lowercase letters, digits or \"-\"", f.Name)
	}
	if f.Sessions != nil {
		return checkConversations(f)
	}
	u, err := url.Parse(f.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || strings.ContainsFunc(f.URL, unicode.IsControl) {
		return fmt.Errorf("the url of %s is not an http or https URL with no user, query or fragment", f.Name)
	}
	if f.Owner == "" || len(f.Owner) > MaxMemoryOwner || strings.ContainsFunc(f.Owner, unicode.IsControl) {
		return fmt.Errorf("the owner of %s is not 1 to %d bytes with no control character", f.Name, MaxMemoryOwner)
	}
	if f.Path == "" || len(f.Path) > MaxMemoryPath || strings.ContainsFunc(f.Path, unicode.IsControl) {
		return fmt.Errorf("the path of %s is not a relative path of 1 to %d bytes with no control character", f.Name, MaxMemoryPath)
	}
	for seg := range strings.SplitSeq(f.Path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("the path of %s holds the segment %q", f.Name, seg)
		}
	}
	if f.Access != MemoryReadOnly && f.Access != MemoryReadWrite {
		return fmt.Errorf("the access of %s is %q, not %s or %s", f.Name, f.Access, MemoryReadOnly, MemoryReadWrite)
	}
	return nil
}

// checkConversations holds a folder of conversations to spec 094: no
// files store's address, read only, and at most one metadata entry, its
// key and its value each a metadata entry's.
func checkConversations(f MemoryFolder) error {
	switch {
	case f.URL != "" || f.Owner != "" || f.Path != "":
		return fmt.Errorf("%s names sessions and a files store's url, owner or path; a folder holds one or the other", f.Name)
	case f.Access != MemoryReadOnly:
		return fmt.Errorf("the access of %s is %q; a folder of conversations is %s", f.Name, f.Access, MemoryReadOnly)
	case len(f.Sessions.Metadata) > MaxMemorySessionsMetadata:
		return fmt.Errorf("the sessions of %s name %d metadata entries, more than %d", f.Name, len(f.Sessions.Metadata), MaxMemorySessionsMetadata)
	}
	for k, v := range f.Sessions.Metadata {
		if !ValidMetadataKey(k) {
			return fmt.Errorf("the sessions of %s name the metadata key %q, which is not %s", f.Name, k, MetadataKeyRule)
		}
		if v == nil {
			continue
		}
		if why := CheckMetadataValue(*v); why != "" {
			return fmt.Errorf("the sessions of %s name a value of %s that cannot be a metadata value: %s", f.Name, k, why)
		}
	}
	return nil
}

// CloneMemory is a deep copy of fs: a folder of conversations' metadata
// entry is copied with it, so a change to the copy changes nothing of fs.
func CloneMemory(fs []MemoryFolder) []MemoryFolder {
	if fs == nil {
		return nil
	}
	out := slices.Clone(fs)
	for i, f := range out {
		if f.Sessions == nil {
			continue
		}
		c := MemorySessions{}
		if f.Sessions.Metadata != nil {
			c.Metadata = make(map[string]*string, len(f.Sessions.Metadata))
			for k, v := range f.Sessions.Metadata {
				if v != nil {
					c.Metadata[k] = new(*v)
				} else {
					c.Metadata[k] = nil
				}
			}
		}
		out[i].Sessions = &c
	}
	return out
}

// MemoryAnswer is a session's memory folders as the API answers them
// (spec 093): each by its name and access, without the files API's
// address, its space or its path.
func MemoryAnswer(fs []MemoryFolder) []MemoryFolder {
	if len(fs) == 0 {
		return nil
	}
	out := make([]MemoryFolder, len(fs))
	for i, f := range fs {
		out[i] = MemoryFolder{Name: f.Name, Access: f.Access}
	}
	return out
}
