// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

// Command sandbox shows how a host selects the execution backend. By default
// the runtime uses the local temp-directory provider, so this runs offline. Set
// TOPOS_CELLA_TOKEN to instead run on hosted Cella compute, at
// https://api.latere.ai/v1/environments unless TOPOS_CELLA_URL names another
// Cella control plane, demonstrating that swapping backends is a one-line
// Options change and nothing else in the program differs.
//
//	go run ./examples/sandbox                         # local
//	TOPOS_CELLA_TOKEN=... go run ./examples/sandbox   # hosted
//	TOPOS_CELLA_URL=https://cella.example.com/v1/environments \
//	TOPOS_CELLA_TOKEN=... go run ./examples/sandbox   # another control plane
package main

import (
	"context"
	"fmt"
	"os"

	"latere.ai/x/topos"
	"latere.ai/x/topos/sandbox"
	"latere.ai/x/topos/sandbox/cella"
)

// defaultCellaURL is the hosted Cella control plane, served under the platform
// origin.
const defaultCellaURL = "https://api.latere.ai/v1/environments"

func run() error {
	opts := topos.Options{
		SessionID: "sandbox-demo",
		Model:     topos.ModelOptions{Kind: topos.ModelFake},
	}

	// Choose the backend. When Options.Sandbox is nil the runner falls back to
	// the local provider, so the default path needs no services. A token for
	// Cella swaps in hosted compute with no other change to the program.
	ctx := context.Background()
	url, token := os.Getenv("TOPOS_CELLA_URL"), os.Getenv("TOPOS_CELLA_TOKEN")
	if url != "" || token != "" {
		if url == "" {
			url = defaultCellaURL
		}
		opts.Sandbox = cella.New(cella.Options{
			BaseURL: url,
			Token:   cella.StaticTokenSource(token),
		})
		// Cella scopes work to the bearer carried on the context.
		ctx = sandbox.WithBearer(ctx, token)
		fmt.Println("backend: cella", url)
	} else {
		fmt.Println("backend: local (set TOPOS_CELLA_TOKEN to use hosted Cella)")
	}

	r, err := topos.NewRunner(opts)
	if err != nil {
		return fmt.Errorf("new runner: %w", err)
	}

	res, err := r.Run(ctx, topos.Region{
		Autonomy: topos.Pinned,
		Entry:    topos.AgentSpec{Name: "solo", Role: "solo", Tools: []string{"bash"}},
	}, "say hello")
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}

	fmt.Println("final:", res.Final)
	for _, n := range res.Trace.Nodes {
		fmt.Printf("ran %s in sandbox %s\n", n.ID, n.Sandbox)
	}

	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
