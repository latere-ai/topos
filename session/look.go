// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"fmt"
	"strings"
	"time"

	"latere.ai/x/topos/prompts"
)

// Look is a turn a trigger's message began (spec 087): the firing that
// sent the message, the trigger it was sent as, whether it was a wake's,
// when the message was appended, and what the turn cost. Its cost is what
// the log's events from the message through the status that closes the
// turn of the session's own thread add to the session's spend, in every
// thread, ended earlier by the next user.message of that thread; a turn
// still running counts what it has spent so far and is not Ended.
type Look struct {
	FiringID     string
	TriggerID    string
	Wake         bool
	At           time.Time
	CostUSDMicro int64
	Ended        bool
}

// Looks are the looks of a session's log in order. Events a fork copied
// from its parent are its parent's looks and are left out.
func Looks(s Session, evs []Event) []Look {
	var out []Look
	open := -1
	for _, e := range evs {
		if s.Copied(e) {
			continue
		}
		if e.Type == TypeUserMessage && e.Thread == "" && !e.Redacted() {
			if open >= 0 {
				out[open].Ended = true
				open = -1
			}
			var p UserMessage
			if e.Decode(&p) == nil && p.FiringID != "" {
				out = append(out, Look{FiringID: p.FiringID, TriggerID: strings.TrimPrefix(p.Sender.Subject, TriggerSubjectPrefix), Wake: p.Wake != nil, At: e.Time})
				open = len(out) - 1
			}
			continue
		}
		if open < 0 {
			continue
		}
		out[open].CostUSDMicro += Cost(e)
		if e.Type == TypeSessionStatus && e.Thread == "" && !e.Redacted() {
			var p SessionStatus
			if e.Decode(&p) == nil && p.Status != "" && p.Status != StatusRunning {
				out[open].Ended = true
				open = -1
			}
		}
	}
	return out
}

// MessageSpend is the spend a firing's user.message carries (spec 087):
// the week of the trigger the look counts toward, as the server counted it
// at the firing, before this look. TriggerID is that trigger: the
// sender's, or, for a wake, the trigger that started the session.
type MessageSpend struct {
	TriggerID         string `json:"trigger_id"`
	WindowDays        int    `json:"window_days"`
	SpentUSDMicro     int64  `json:"spent_usd_micro"`
	PerWeekUSDMicro   *int64 `json:"per_week_usd_micro,omitempty"`
	PerLookUSDMicro   *int64 `json:"per_look_usd_micro,omitempty"`
	ExpectedLooks     int    `json:"expected_looks"`
	ProjectedUSDMicro *int64 `json:"projected_usd_micro,omitempty"`
}

// Ahead reports whether the watch is ahead of pace: its window is on
// course to cost more than its allowance.
func (m MessageSpend) Ahead() bool {
	return m.PerWeekUSDMicro != nil && m.ProjectedUSDMicro != nil && *m.ProjectedUSDMicro > *m.PerWeekUSDMicro
}

// USD is an amount of USD micros as a person reads it: to the cent, and
// to a hundredth of a cent below one cent, so a cheap look reads as more
// than nothing.
func USD(micro int64) string {
	if micro != 0 && micro < 10000 && micro > -10000 {
		return fmt.Sprintf("$%.4f", float64(micro)/1e6)
	}
	return fmt.Sprintf("$%.2f", float64(micro)/1e6)
}

// paceLine is the line the fold renders before a firing's message that
// carries its spend (spec 087); wake says whether a wake sent it.
func paceLine(m MessageSpend, wake bool) string {
	amount := func(p *int64) string {
		if p == nil {
			return ""
		}
		return USD(*p)
	}
	return prompts.Render(prompts.ContextPace, prompts.Data{
		"Spent": USD(m.SpentUSDMicro), "PerWeek": amount(m.PerWeekUSDMicro), "PerLook": amount(m.PerLookUSDMicro),
		"Projected": amount(m.ProjectedUSDMicro), "Looks": m.ExpectedLooks, "Ahead": m.Ahead(), "Wake": wake,
	})
}
