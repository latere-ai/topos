// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package agent

import "os/exec"

func setProcessGroup(_ *exec.Cmd)            {}
func signalProcessGroup(_ *exec.Cmd, _ bool) {}
