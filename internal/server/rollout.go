// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

// RunRule is the sentence the create route states a rollout's run and a
// model named at create by (spec 077, R1 and R3).
const RunRule = "The body's rollout, the id of the rollout the session is one run of, " +
	"a letter or digit then at most 127 letters, digits, dots, underscores or hyphens, and neither none nor any, " +
	"is the session's for its whole life, and session.create carries it as rollout. " +
	"A rollout's runs are left out of the session list, the search and the summary unless their rollout parameter names the rollout or any, " +
	"and no person attends them: a call that would wait for a person's confirmation is answered at once as denied, and the session does not wait. " +
	"A run reaches none of its initiator's accounts or context: whatever its allow names, it holds no instructions, no context, no attached repository and no connection, " +
	"a send's allow changes no connection of it, its sandbox holds no token of the connection service, and none is minted for it; " +
	"its network, its folder and the repositories its body names are its allow's and its body's, as any session's. " +
	"The body's model {name, reasoning}, either member or both, names the model and the reasoning level the session runs from its first request in place of its agent's; " +
	"session.create carries them as model, and as reasoning and effort, and an allow that names another model or another level is model_not_allowed, " +
	"so the session runs as named or not at all."

// RolloutFilterRule is the sentence the list routes state their rollout
// filter by (spec 077).
const RolloutFilterRule = "rollout=<id>, a rollout's id, keeps that rollout's runs, and rollout=any every session; " +
	"absent, the runs of every rollout are left out, beside every other filter. Any other value is invalid_request."
