// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"

	"latere.ai/x/topos/authorizer"
)

// table is the API's route table (spec 015): the router, the test that
// every route asks its action, and the OpenAPI document are all built
// from it.
func table() []route {
	a := func(actions ...string) []string { return actions }
	return []route{
		{method: http.MethodPut, path: "/agents/{name}", actions: a(authorizer.ActionAgentCreate, authorizer.ActionAgentUpdate, authorizer.ActionAgentRead),
			op: "applyAgent", summary: "Apply an agent", status: http.StatusOK, creates: true, body: MaxBody, handle: (*call).applyAgent},
		{method: http.MethodGet, path: "/agents", actions: a(authorizer.ActionAgentList),
			op: "listAgents", summary: "List agents", status: http.StatusOK, handle: (*call).listAgents},
		{method: http.MethodGet, path: "/agents/{ref}", actions: a(authorizer.ActionAgentRead),
			op: "getAgent", summary: "Read an agent", status: http.StatusOK, handle: (*call).getAgent},
		{method: http.MethodGet, path: "/agents/{ref}/versions", actions: a(authorizer.ActionAgentRead),
			op: "listAgentVersions", summary: "List agent versions", status: http.StatusOK, handle: (*call).listAgentVersions},
		{method: http.MethodGet, path: "/agents/{ref}/versions/{n}", actions: a(authorizer.ActionAgentRead),
			op: "getAgentVersion", summary: "Read an agent version", status: http.StatusOK, handle: (*call).getAgentVersion},
		{method: http.MethodPost, path: "/agents/{ref}/archive", actions: a(authorizer.ActionAgentArchive),
			op: "archiveAgent", summary: "Archive an agent", status: http.StatusOK, body: MaxBody, handle: (*call).archiveAgent},
		{method: http.MethodPost, path: "/sessions", actions: a(authorizer.ActionSessionCreate),
			op: "createSession", summary: "Create a session", status: http.StatusCreated, body: MaxBody, handle: (*call).createSession},
		{method: http.MethodGet, path: "/sessions", actions: a(authorizer.ActionSessionList),
			op: "listSessions", summary: "List sessions", status: http.StatusOK, handle: (*call).listSessions},
		{method: http.MethodGet, path: "/sessions/summary", actions: a(authorizer.ActionSessionList),
			op: "getSessionSummary", summary: "Count sessions", status: http.StatusOK, handle: (*call).getSessionSummary},
		{method: http.MethodGet, path: "/sessions/{id}", actions: a(authorizer.ActionSessionRead),
			op: "getSession", summary: "Read a session", status: http.StatusOK, handle: (*call).getSession},
		{method: http.MethodPatch, path: "/sessions/{id}", actions: a(authorizer.ActionSessionUpdate, authorizer.ActionSessionRead),
			op: "updateSession", summary: "Change a session's model", status: http.StatusOK, body: MaxBody, handle: (*call).updateSession},
		{method: http.MethodPost, path: "/sessions/{id}/end", actions: a(authorizer.ActionSessionEnd, authorizer.ActionSessionRead),
			op: "endSession", summary: "End a session", status: http.StatusOK, body: MaxBody, handle: (*call).endSession},
		{method: http.MethodPost, path: "/sessions/{id}/fork", actions: a(authorizer.ActionSessionRead, authorizer.ActionSessionFork),
			op: "forkSession", summary: "Fork a session", status: http.StatusCreated, body: MaxBody, handle: (*call).forkSession},
		{method: http.MethodPost, path: "/sessions/{id}/archive", actions: a(authorizer.ActionSessionRead, authorizer.ActionSessionUpdate),
			op: "archiveSession", summary: "Archive a session", status: http.StatusOK, body: MaxBody, handle: (*call).archiveSession},
		{method: http.MethodPost, path: "/sessions/{id}/unarchive", actions: a(authorizer.ActionSessionRead, authorizer.ActionSessionUpdate),
			op: "unarchiveSession", summary: "Unarchive a session", status: http.StatusOK, body: MaxBody, handle: (*call).unarchiveSession},
		{method: http.MethodPost, path: "/sessions/{id}/resume", actions: a(authorizer.ActionSessionResume),
			op: "resumeSession", summary: "Resume a session", status: http.StatusOK, body: MaxBody, handle: (*call).resumeSession},
		{method: http.MethodDelete, path: "/sessions/{id}", actions: a(authorizer.ActionSessionDelete, authorizer.ActionSessionRead),
			op: "deleteSession", summary: "Delete a session", status: http.StatusNoContent, handle: (*call).deleteSession},
		{method: http.MethodGet, path: "/sessions/{id}/events", actions: a(authorizer.ActionSessionRead),
			op: "listEvents", summary: "List session events", status: http.StatusOK, handle: (*call).listEvents},
		{method: http.MethodPost, path: "/sessions/{id}/events", actions: a(authorizer.ActionSessionSend, authorizer.ActionSessionInterrupt),
			op: "sendEvent", summary: "Send a user event", status: http.StatusOK, body: MaxEventBody, handle: (*call).sendEvent},
		{method: http.MethodGet, path: "/sessions/{id}/stream", actions: a(authorizer.ActionSessionRead),
			op: "streamEvents", summary: "Stream session events", status: http.StatusOK, handle: (*call).stream},
		{method: http.MethodGet, path: "/sessions/{id}/blobs/{digest}", actions: a(authorizer.ActionSessionRead),
			op: "getBlob", summary: "Read a session blob", status: http.StatusOK, handle: (*call).blob},
		{method: http.MethodGet, path: "/sessions/{id}/files", actions: a(authorizer.ActionSessionRead),
			op: "getFile", summary: "Read a session file", status: http.StatusOK, handle: (*call).file},
		{method: http.MethodPost, path: "/sessions/{id}/events/{event_id}/redact", actions: a(authorizer.ActionSessionRedact),
			op: "redactEvent", summary: "Redact an event", status: http.StatusNoContent, body: MaxBody, handle: (*call).redact},
		{method: http.MethodPut, path: "/triggers/{name}", actions: a(authorizer.ActionTriggerCreate, authorizer.ActionTriggerUpdate, authorizer.ActionAgentRead),
			op: "applyTrigger", summary: "Apply a trigger", status: http.StatusOK, creates: true, body: MaxBody, handle: (*call).applyTrigger},
		{method: http.MethodGet, path: "/triggers", actions: a(authorizer.ActionTriggerList),
			op: "listTriggers", summary: "List triggers", status: http.StatusOK, handle: (*call).listTriggers},
		{method: http.MethodGet, path: "/triggers/{ref}", actions: a(authorizer.ActionTriggerRead),
			op: "getTrigger", summary: "Read a trigger", status: http.StatusOK, handle: (*call).getTrigger},
		{method: http.MethodDelete, path: "/triggers/{ref}", actions: a(authorizer.ActionTriggerDelete),
			op: "deleteTrigger", summary: "Delete a trigger", status: http.StatusNoContent, handle: (*call).deleteTrigger},
		// A firing asks session.create and session.send as the trigger's
		// owner, not as the caller (spec 022).
		{method: http.MethodPost, path: "/triggers/{ref}/fire", actions: a(authorizer.ActionTriggerFire, authorizer.ActionSessionCreate, authorizer.ActionSessionSend),
			op: "fireTrigger", summary: "Fire a trigger", status: http.StatusOK, body: MaxBody, handle: (*call).fireTrigger},
		{method: http.MethodGet, path: "/triggers/{ref}/firings", actions: a(authorizer.ActionTriggerRead),
			op: "listFirings", summary: "List trigger firings", status: http.StatusOK, handle: (*call).listFirings},
		{method: http.MethodGet, path: "/openapi.yaml", public: true,
			op: "getOpenAPI", summary: "Read the API document", status: http.StatusOK, handle: (*call).openAPI},
	}
}
