// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestIdentifiersArePrefixedULIDs(t *testing.T) {
	for _, p := range []string{PrefixAgent, PrefixSession, PrefixEvent, PrefixTrigger, PrefixCredential, PrefixMemory} {
		id := NewID(p)
		if err := CheckID(p, id); err != nil {
			t.Fatalf("NewID(%q) = %q: %v", p, id, err)
		}
	}
	for _, bad := range []string{"", "ses_", "evt_01J9Z3Q4W8KX6T0M2V5N7R1B3C", "ses_81J9Z3Q4W8KX6T0M2V5N7R1B3C", "ses_01J9Z3Q4W8KX6T0M2V5N7R1B3U", "ses_01J9Z3Q4W8KX6T0M2V5N7R1B3", "ses_../../etc/passwd"} {
		if err := CheckID(PrefixSession, bad); !errors.Is(err, ErrBadID) {
			t.Fatalf("CheckID(%q) = %v, want ErrBadID", bad, err)
		}
	}
}

func TestNewIDIsMonotonicWithinAMillisecond(t *testing.T) {
	fixed := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	old := nowFunc
	nowFunc = func() time.Time { return fixed }
	defer func() { nowFunc = old }()
	prev := NewID(PrefixEvent)
	for range 1000 {
		next := NewID(PrefixEvent)
		if next <= prev {
			t.Fatalf("%s after %s", next, prev)
		}
		if !strings.HasPrefix(next[len(PrefixEvent):], prev[len(PrefixEvent):len(PrefixEvent)+10]) {
			t.Fatalf("the time part changed within one millisecond: %s, %s", prev, next)
		}
		prev = next
	}
}

func TestIDsAreMintedAndReadOnlyInUppercase(t *testing.T) {
	id := NewID(PrefixSession)
	if rest := strings.TrimPrefix(id, PrefixSession); len(rest) != 26 || rest != strings.ToUpper(rest) {
		t.Fatalf("NewID = %q, want 26 uppercase characters after the prefix", id)
	}
	if err := CheckID(PrefixSession, id); err != nil {
		t.Fatalf("CheckID(%q) = %v", id, err)
	}
	lower := PrefixSession + strings.ToLower(strings.TrimPrefix(id, PrefixSession))
	if err := CheckID(PrefixSession, lower); !errors.Is(err, ErrBadID) {
		t.Fatalf("CheckID(%q) = %v, want ErrBadID: a lowercase spelling would alias the id's directory", lower, err)
	}
	if _, err := MintedAt(PrefixSession, lower); !errors.Is(err, ErrBadID) {
		t.Fatalf("MintedAt(%q) = %v, want ErrBadID", lower, err)
	}
}

func TestMintedAtReadsTheIDsTime(t *testing.T) {
	before := time.Now().Truncate(time.Millisecond)
	id := NewID(PrefixSession)
	at, err := MintedAt(PrefixSession, id)
	if err != nil || at.Before(before) || at.After(time.Now()) {
		t.Fatalf("MintedAt(%s) = %v, %v", id, at, err)
	}
	if _, err := MintedAt(PrefixSession, "ses_nope"); !errors.Is(err, ErrBadID) {
		t.Fatalf("a bad id: %v", err)
	}
}
