// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"

	"latere.ai/x/topos/internal/config"
	"latere.ai/x/topos/internal/hosted"
)

// hostSessions refuses TOPOS_HOST_SESSIONS=on: a server's host sessions
// run every command in the host sandbox, which has no driver on
// Windows, and a server session never runs without one (spec 009).
func hostSessions(context.Context, config.Config, config.Getenv) (hosted.Machines, error) {
	return nil, errors.New("the host sandbox has no driver on windows, and a server runs no session on its host without one; leave TOPOS_HOST_SESSIONS off and run hosted sessions on Cella with TOPOS_CELLA_URL")
}
