// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package prompt names the harness prompt's options for the callers that
// import them from this path. The harness prompt, its versions and every
// other text a model reads are in latere.ai/x/topos/prompts.
package prompt

import "latere.ai/x/topos/prompts"

// Options are the facts the harness prompt's conditional sections depend
// on.
type Options = prompts.HarnessOptions
