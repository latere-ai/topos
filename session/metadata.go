// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"unicode"
)

// MaxMetadataValue is the longest value of a session's metadata entry a
// create or a change writes, in bytes (spec 057).
const MaxMetadataValue = 512

// metadataKey is the form of a metadata key a create or a change writes:
// one a query parameter's name can carry after "metadata." (spec 057).
var metadataKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// MetadataKeyRule states metadataKey for a refusal.
const MetadataKeyRule = "a letter or digit, then at most 62 letters, digits, dots, underscores or hyphens"

// ValidMetadataKey reports whether key has the form a metadata key a
// create or a change writes takes.
func ValidMetadataKey(key string) bool { return metadataKey.MatchString(key) }

// CheckMetadataValue reports why v cannot be a metadata value, "" when
// it can: longer than MaxMetadataValue bytes, or holding a control
// character.
func CheckMetadataValue(v string) string {
	if len(v) > MaxMetadataValue {
		return fmt.Sprintf("the value is %d bytes, more than %d", len(v), MaxMetadataValue)
	}
	for _, r := range v {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return fmt.Sprintf("the value holds the control character %U", r)
		}
	}
	return ""
}

// CheckMetadata reports why m cannot be the metadata a create or a change
// leaves a session with: more than MaxMetadata entries, a key outside the
// rule, or a value CheckMetadataValue refuses. The entries are checked in
// key order, so a refusal names the same entry every time.
func CheckMetadata(m map[string]string) error {
	if len(m) > MaxMetadata {
		return fmt.Errorf("%d metadata entries, at most %d", len(m), MaxMetadata)
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if !ValidMetadataKey(k) {
			return fmt.Errorf("metadata key %q is not %s", k, MetadataKeyRule)
		}
		if why := CheckMetadataValue(m[k]); why != "" {
			return fmt.Errorf("metadata %q: %s", k, why)
		}
	}
	return nil
}

// MetadataEntry is one metadata entry a List keeps sessions by: those
// whose metadata holds Key with exactly Value (spec 057).
type MetadataEntry struct {
	Key   string
	Value string
}

// MergeMetadata answers m with change applied, a value setting its key
// and nil deleting it, and whether that changed anything. m is not
// modified; an empty result is nil, as a session with no metadata holds.
func MergeMetadata(m map[string]string, change map[string]*string) (map[string]string, bool) {
	out := maps.Clone(m)
	changed := false
	for k, v := range change {
		old, had := out[k]
		switch {
		case v == nil && had:
			delete(out, k)
			changed = true
		case v != nil && (!had || old != *v):
			if out == nil {
				out = map[string]string{}
			}
			out[k] = *v
			changed = true
		}
	}
	if len(out) == 0 {
		out = nil
	}
	return out, changed
}

// Labeler is the optional interface of a store that changes a session's
// metadata without an event (spec 057): SetMetadata merges change into
// the session's metadata under its lock, as MergeMetadata does, and
// answers the session as it is after. A change that leaves the metadata
// as it was writes nothing, and a merge past MaxMetadata entries is
// ErrInvalid and writes nothing.
type Labeler interface {
	SetMetadata(ctx context.Context, id string, change map[string]*string) (Session, error)
}

// Relabel applies change to s's metadata in place, as a Labeler's
// SetMetadata does, and reports whether that changed anything.
func Relabel(s *Session, change map[string]*string) (bool, error) {
	next, changed := MergeMetadata(s.Metadata, change)
	if !changed {
		return false, nil
	}
	if len(next) > MaxMetadata {
		return false, fmt.Errorf("%w: %d metadata entries, at most %d", ErrInvalid, len(next), MaxMetadata)
	}
	s.Metadata = next
	return true, nil
}
