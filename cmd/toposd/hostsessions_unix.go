// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"

	"latere.ai/x/topos/internal/config"
	"latere.ai/x/topos/internal/hosted"
)

// hostSessions opens the host sessions of TOPOS_HOST_SESSIONS=on: the
// srt driver, checked by its preflight and a probe before the server
// starts.
func hostSessions(ctx context.Context, cfg config.Config, getenv config.Getenv) (hosted.Machines, error) {
	driver, err := hosted.SandboxDriver(getenv)
	if err != nil {
		return nil, err
	}
	// Every file the configuration names is denied to commands, the
	// Cella bearer's among them.
	var denied []string
	for _, p := range []string{cfg.CellaTokenFile, cfg.MachineHelpers} {
		if p != "" {
			denied = append(denied, p)
		}
	}
	return hosted.NewHost(ctx, hosted.HostOptions{DataDir: cfg.DataDir, Denied: denied, Driver: driver})
}
