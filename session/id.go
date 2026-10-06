// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"strings"
	"sync"
	"time"
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

// crockford is the ULID alphabet: Crockford's base32, uppercase, without
// I, L, O and U.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ulidLen is the length of a ULID in characters: 48 bits of time and 80
// bits of randomness, 26 base32 characters.
const ulidLen = 26

var (
	idMu      sync.Mutex
	lastMilli uint64
	lastRand  [10]byte
	nowFunc   = time.Now
)

// NewID returns a fresh id with the given prefix. Ids minted in the same
// millisecond by one process increase monotonically, so a log's event ids
// sort in the order they were minted.
func NewID(prefix string) string {
	idMu.Lock()
	defer idMu.Unlock()
	ms := uint64(nowFunc().UnixMilli())
	if ms == lastMilli {
		increment(&lastRand)
	} else {
		lastMilli = ms
		if _, err := rand.Read(lastRand[:]); err != nil {
			// crypto/rand never fails on the platforms Go supports; a
			// failure here is a broken host, and an id must never repeat.
			panic("session: crypto/rand: " + err.Error())
		}
	}
	var b [16]byte
	binary.BigEndian.PutUint16(b[0:2], uint16(ms>>32))
	binary.BigEndian.PutUint32(b[2:6], uint32(ms))
	copy(b[6:], lastRand[:])
	return prefix + encodeULID(b)
}

// increment adds one to the 80-bit random part, carrying.
func increment(r *[10]byte) {
	for i := len(r) - 1; i >= 0; i-- {
		r[i]++
		if r[i] != 0 {
			return
		}
	}
}

// encodeULID renders 128 bits as 26 Crockford base32 characters, the
// first carrying the top 3 bits.
func encodeULID(b [16]byte) string {
	var out [ulidLen]byte
	// Treat the 128 bits as a big number and take 5 bits at a time from
	// the least significant end.
	var hi, lo uint64
	hi = binary.BigEndian.Uint64(b[0:8])
	lo = binary.BigEndian.Uint64(b[8:16])
	for i := ulidLen - 1; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}

// ErrBadID is returned by CheckID for a string that is not a prefixed ULID.
var ErrBadID = errors.New("not a prefixed ULID")

// CheckID reports whether id is prefix followed by a ULID.
func CheckID(prefix, id string) error {
	rest, ok := strings.CutPrefix(id, prefix)
	if !ok || len(rest) != ulidLen {
		return ErrBadID
	}
	// The first character carries 3 bits of the 128, so it is at most 7.
	if rest[0] > '7' {
		return ErrBadID
	}
	for i := range len(rest) {
		if !strings.ContainsRune(crockford, rune(rest[i])) {
			return ErrBadID
		}
	}
	return nil
}

// MintedAt is the time an id was minted: the first 48 bits of its ULID,
// which its first ten characters carry.
func MintedAt(prefix, id string) (time.Time, error) {
	if err := CheckID(prefix, id); err != nil {
		return time.Time{}, err
	}
	var ms int64
	for _, c := range id[len(prefix) : len(prefix)+10] {
		ms = ms<<5 | int64(strings.IndexRune(crockford, c))
	}
	return time.UnixMilli(ms), nil
}
