// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

// Permission is one grant of the agent's key (specs 006 and 018): an
// action of the vocabulary and the resource it may be taken on. It is a
// grant a manifest declares, not a question put to an authorizer, and it
// sits in a file of its own so the identity gate's exemption for it
// covers this type alone.
type Permission struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
}
