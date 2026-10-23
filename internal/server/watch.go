// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"

	"latere.ai/x/topos/manifest/trigger"
	"latere.ai/x/topos/session"
)

// ContextRule is how a session's log and header say how full its context
// is (spec 087), which the event routes' descriptions carry.
var ContextRule = fmt.Sprintf("Each model.request of a thread's step or a compaction records context {window, used, source, parts}: window the model's input window from the catalog, 0 when it names none; "+
	"used the input tokens the request used, source %s when the response counted its prompt tokens and %s when it did not, as a failed or cut request, where used is the harness's estimate; "+
	"and parts [{part, tokens}] in the order %s, %s, %s, %s, %s, %s, each the harness's estimate scaled so the parts but %s sum to used, and %s the window less used. "+
	"A title's request records none. The session folds the latest of its own thread's as context_report.",
	session.ContextFromProvider, session.ContextFromEstimate,
	session.ReportInstructions, session.ReportTools, session.ReportMessages, session.ReportFiles, session.ReportSummary, session.ReportFree, session.ReportFree, session.ReportFree)

// WatchedRule is how the log says whether a person watched a decision
// (spec 087), which the event routes' descriptions carry.
const WatchedRule = "agent.tool_use and approval.requested record attended: false on a turn a trigger's message began, a wake's included, and in a subagent's thread that turn drove; " +
	"true on a turn a person's message, answer or confirmation began. Such a turn decides without the patterns the session's confirmations remembered; " +
	"always_allow, always_confirm, the mode and a decision service decide as on any turn, and a decision service reads attended in the action's attributes."

// SpendRule is the allowance a trigger may name and the spend a firing's
// message carries (spec 087), which the trigger routes' descriptions
// carry.
var SpendRule = fmt.Sprintf("spec.budget.perWeek, an integer of USD micros above zero, is the most the looks counted toward the trigger may cost in a rolling %d days: "+
	"a look is a turn a firing's message began, counted from that message through the turn's closing status, and the looks counted are the trigger's own, in the sessions it started or continued, "+
	"and the ones wakes began in the sessions it started. A firing while that spend is at or past the allowance starts nothing and records skipped_budget, counted as skippedBudget; "+
	"a one-shot, a wake included, keeps its time and is tried again each minute until it fires or maxAge passes, and no firing needs a resume. trigger.create and trigger.update carry budget {per_week_usd_micro}. "+
	"status.spend {windowDays, spentUsdMicro, perWeekUsdMicro, perLookUsdMicro, expectedLooks, projectedUsdMicro} is written at each firing that counts toward the trigger, before its look: "+
	"perLookUsdMicro the mean of the window's latest %d looks that ended, expectedLooks the looks expected from the firing to the end of the window that the oldest counted look opened, "+
	"and projectedUsdMicro spent plus perLook times expectedLooks. The firing's user.message carries the same numbers as spend {trigger_id, window_days, spent_usd_micro, per_week_usd_micro, per_look_usd_micro, expected_looks, projected_usd_micro}, "+
	"trigger_id the trigger counted toward, for a wake the trigger that started its session. A session's trigger tool writes spec.budget.perWeek from its perWeekUsd, the agent's estimate of a week of looks.",
	trigger.SpendWindowDays, trigger.PaceLooks)
