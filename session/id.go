// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"errors"
	"time"

	"latere.ai/x/pkg/ulid"
)

// The kind prefixes of spec 004. Every object id is a ULID behind one of
// them; nothing else carries a prefix.
const (
	PrefixAgent      = "agent_"
	PrefixSession    = "ses_"
	PrefixEvent      = "evt_"
	PrefixTrigger    = "trg_"
	PrefixFiring     = "frg_"
	PrefixCredential = "cred_"
	PrefixMemory     = "mem_"
	// PrefixApproval is an approval.requested's approval_id, minted by the
	// runner (spec 052).
	PrefixApproval = "apr_"
)

// nowFunc is the clock ids are minted by.
var nowFunc = time.Now

// ids mints in uppercase: stored ids keep the case they were minted in.
var ids = ulid.Generator{Now: func() time.Time { return nowFunc() }, Upper: true}

// NewID returns a fresh id with the given prefix. Ids minted in the same
// millisecond by one process increase monotonically, so a log's event ids
// sort in the order they were minted.
func NewID(prefix string) string { return ids.New(prefix) }

// ErrBadID is returned by CheckID for a string that is not a prefixed ULID.
var ErrBadID = errors.New("not a prefixed ULID")

// CheckID reports whether id is prefix followed by a ULID in either case.
func CheckID(prefix, id string) error {
	if !ulid.Valid(prefix, id) {
		return ErrBadID
	}
	return nil
}

// MintedAt is the time an id was minted: the first 48 bits of its ULID.
func MintedAt(prefix, id string) (time.Time, error) {
	if err := CheckID(prefix, id); err != nil {
		return time.Time{}, err
	}
	at, ok := ulid.Time(id)
	if !ok {
		return time.Time{}, ErrBadID
	}
	return at, nil
}
