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
			op: "applyAgent", summary: "Apply an Agent manifest to the caller's own agent of the name; a changed spec creates a version", status: http.StatusOK, body: MaxBody, handle: (*call).applyAgent},
		{method: http.MethodGet, path: "/agents", actions: a(authorizer.ActionAgentList),
			op: "listAgents", summary: "List agents", status: http.StatusOK, handle: (*call).listAgents},
		{method: http.MethodGet, path: "/agents/{ref}", actions: a(authorizer.ActionAgentRead),
			op: "getAgent", summary: "Get an agent's latest version by id, or by name among the caller's own agents", status: http.StatusOK, handle: (*call).getAgent},
		{method: http.MethodGet, path: "/agents/{ref}/versions", actions: a(authorizer.ActionAgentRead),
			op: "listAgentVersions", summary: "List an agent's versions", status: http.StatusOK, handle: (*call).listAgentVersions},
		{method: http.MethodGet, path: "/agents/{ref}/versions/{n}", actions: a(authorizer.ActionAgentRead),
			op: "getAgentVersion", summary: "Get one version of an agent", status: http.StatusOK, handle: (*call).getAgentVersion},
		{method: http.MethodPost, path: "/agents/{ref}/archive", actions: a(authorizer.ActionAgentArchive),
			op: "archiveAgent", summary: "Archive an agent; running sessions keep their version", status: http.StatusOK, body: MaxBody, handle: (*call).archiveAgent},
		{method: http.MethodPost, path: "/sessions", actions: a(authorizer.ActionSessionCreate),
			op: "createSession", summary: "Create a session of an agent, named by id or by name among the caller's own agents", status: http.StatusCreated, body: MaxBody, handle: (*call).createSession},
		{method: http.MethodGet, path: "/sessions", actions: a(authorizer.ActionSessionList),
			op: "listSessions", summary: "List sessions, filtered by agent, status and runner", status: http.StatusOK, handle: (*call).listSessions},
		{method: http.MethodGet, path: "/sessions/{id}", actions: a(authorizer.ActionSessionRead),
			op: "getSession", summary: "Get a session", status: http.StatusOK, handle: (*call).getSession},
		{method: http.MethodPost, path: "/sessions/{id}/end", actions: a(authorizer.ActionSessionEnd),
			op: "endSession", summary: "End an idle session completed or canceled", status: http.StatusOK, body: MaxBody, handle: (*call).endSession},
		{method: http.MethodPost, path: "/sessions/{id}/resume", actions: a(authorizer.ActionSessionResume),
			op: "resumeSession", summary: "Resume a session idle on its budget once the cap is raised", status: http.StatusOK, body: MaxBody, handle: (*call).resumeSession},
		{method: http.MethodDelete, path: "/sessions/{id}", actions: a(authorizer.ActionSessionDelete),
			op: "deleteSession", summary: "Delete a session, its log and its blobs", status: http.StatusNoContent, handle: (*call).deleteSession},
		{method: http.MethodGet, path: "/sessions/{id}/events", actions: a(authorizer.ActionSessionRead),
			op: "listEvents", summary: "List a session's events from a sequence", status: http.StatusOK, handle: (*call).listEvents},
		{method: http.MethodPost, path: "/sessions/{id}/events", actions: a(authorizer.ActionSessionSend, authorizer.ActionSessionInterrupt),
			op: "sendEvent", summary: "Send one user event", status: http.StatusOK, body: MaxEventBody, handle: (*call).sendEvent},
		{method: http.MethodGet, path: "/sessions/{id}/stream", actions: a(authorizer.ActionSessionRead),
			op: "streamEvents", summary: "Stream a session's events as Server-Sent Events", status: http.StatusOK, handle: (*call).stream},
		{method: http.MethodGet, path: "/sessions/{id}/blobs/{digest}", actions: a(authorizer.ActionSessionRead),
			op: "getBlob", summary: "Get a blob of a session", status: http.StatusOK, handle: (*call).blob},
		{method: http.MethodPost, path: "/sessions/{id}/events/{event_id}/redact", actions: a(authorizer.ActionSessionRedact),
			op: "redactEvent", summary: "Replace one event's content with a tombstone", status: http.StatusNoContent, body: MaxBody, handle: (*call).redact},
		{method: http.MethodGet, path: "/openapi.yaml", public: true,
			op: "getOpenAPI", summary: "This document", status: http.StatusOK, handle: (*call).openAPI},
	}
}
