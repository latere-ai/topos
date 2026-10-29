// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"

	"github.com/goccy/go-yaml"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/session"
)

// CostRule is how a session's budget counts what its log cost and how a
// client keeps that current from the stream (spec 096), which the
// session's and the event routes' descriptions carry. Its events are
// those session.Cost counts.
var CostRule = fmt.Sprintf("budget.spent_cost_usd_micro is what the session's log cost through last_seq: the cost_usd_micro of every %s, the session's own and each subagent's, a compaction's and a title's included, "+
	"and of every %s a service charged for, such as a %s or a PDF a %s took as text, a redacted result's from its tombstone; no other event adds to it. "+
	"A client that read the session keeps the spending current by adding the cost_usd_micro of each %s and %s after last_seq as the stream carries it, which is the next read's spent_cost_usd_micro. "+
	"A fork's spent_cost_usd_micro counts the events it copied, which carried_cost_usd_micro holds apart: its own spending, which its max_cost_usd_micro caps, is spent less carried.",
	session.TypeModelRequest, session.TypeToolResult, tools.NameWebSearch, tools.NameRead, session.TypeModelRequest, session.TypeToolResult)

// todoResultMeta is the TodoResultMeta schema (specs 008 and 096): the
// to-do list a todo call's tool.result records, which a client reads to
// show how far the agent is through it.
var todoResultMeta = yaml.MapSlice{
	{Key: "type", Value: "object"},
	{Key: "description", Value: fmt.Sprintf("The meta of a %s call's tool.result: the conversation's whole list after the call, which replaces the one before, so the latest todos of a conversation, the session's own or the subagent the event's subagent names, is its list. "+
		"Only a result of outcome %s records it: a call the tool refused, or one that never ran, leaves the list as it was. An emptied list is todos [].", tools.NameTodo, tools.OutcomeOK)},
	{Key: "required", Value: []string{"todos"}},
	{Key: "properties", Value: yaml.MapSlice{
		{Key: "todos", Value: yaml.MapSlice{
			{Key: "type", Value: "array"},
			{Key: "maxItems", Value: tools.TodoLimit},
			{Key: "description", Value: "The items in the agent's order."},
			{Key: "items", Value: yaml.MapSlice{
				{Key: "type", Value: "object"},
				{Key: "required", Value: []string{"id", "content", "status"}},
				{Key: "properties", Value: yaml.MapSlice{
					{Key: "id", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "description", Value: "The item's id, unique in the list."}}},
					{Key: "content", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "description", Value: "The task, in one line."}}},
					{Key: "status", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "enum", Value: []string{tools.TodoPending, tools.TodoInProgress, tools.TodoCompleted}}}},
				}},
			}},
		}},
	}},
}
