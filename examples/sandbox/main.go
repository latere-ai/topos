// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

// Command sandbox shows how a host selects the execution backend. By default
// the runtime uses the local temp-directory provider, so this runs offline. Set
// TOPOS_CELLA_URL (and TOPOS_CELLA_TOKEN) to instead run on hosted Cella
// compute, demonstrating that swapping backends is a one-line Options change and
// nothing else in the program differs.
//
//	go run ./examples/sandbox                       # local
//	TOPOS_CELLA_URL=https://cella.latere.ai \
//	TOPOS_CELLA_TOKEN=... go run ./examples/sandbox  # hosted
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"latere.ai/x/topos"
	"latere.ai/x/topos/sandbox"
	"latere.ai/x/topos/sandbox/cella"
)

func main() {
	opts := topos.Options{
		SessionID: "sandbox-demo",
		Model:     topos.ModelOptions{Kind: topos.ModelFake},
	}

	// Choose the backend. When Options.Sandbox is nil the runner falls back to
	// the local provider, so the default path needs no services. Pointing
	// TOPOS_CELLA_URL at a Cella deployment swaps in hosted compute with no
	// other change to the program.
	ctx := context.Background()
	if url := os.Getenv("TOPOS_CELLA_URL"); url != "" {
		opts.Sandbox = cella.New(cella.Options{
			BaseURL: url,
			Token:   cella.StaticTokenSource(os.Getenv("TOPOS_CELLA_TOKEN")),
		})
		// Cella scopes work to the bearer carried on the context.
		ctx = sandbox.WithBearer(ctx, os.Getenv("TOPOS_CELLA_TOKEN"))
		fmt.Println("backend: cella", url)
	} else {
		fmt.Println("backend: local (set TOPOS_CELLA_URL to use hosted Cella)")
	}

	r, err := topos.NewRunner(opts)
	if err != nil {
		log.Fatalf("new runner: %v", err)
	}

	res, err := r.Run(ctx, topos.Region{
		Autonomy: topos.Pinned,
		Entry:    topos.AgentSpec{Name: "solo", Role: "solo", Tools: []string{"bash"}},
	}, "say hello")
	if err != nil {
		log.Fatalf("run: %v", err)
	}

	fmt.Println("final:", res.Final)
	for _, n := range res.Lineage.Nodes {
		fmt.Printf("ran %s in sandbox %s\n", n.ID, n.Sandbox)
	}
}
