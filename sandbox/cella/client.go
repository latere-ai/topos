// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

// Package cella implements [sandbox.Provider] against a Cella control plane,
// the open source sandbox runtime Latere hosts under
// https://api.latere.ai/v1/environments, through the control plane's exported
// Go client, latere.ai/x/cella/client.
//
// This package is the single place in Topos that knows Cella exists. Per the
// boundary rule (sandbox/boundary_test.go), no package under sandbox/ may
// import it, and the root topos package does not either: a host constructs a
// Provider with [New] and injects it as the [sandbox.Provider] interface.
//
// The exported client's build list is the standard library, Cella's manifest
// types and one error envelope package, so depending on it brings none of the
// control plane's own code into a host binary.
//
// Authentication is bearer-token, supplied per request by a [TokenSource].
package cella

import (
	"errors"
	"fmt"
	"net/http"

	cellaclient "latere.ai/x/cella/client"
	"latere.ai/x/pkg/otel"

	"latere.ai/x/topos/sandbox"
)

// userAgent is the identity every request to the control plane carries.
const userAgent = "topos"

// Options configure a [Provider].
type Options struct {
	// BaseURL is the control plane's address including the base path it is
	// served under, e.g. "https://api.latere.ai/v1/environments". Required.
	BaseURL string
	// Token supplies the bearer token for each request. Required; typically
	// [ContextTokenSource] for user-scoped runs or [StaticTokenSource] for a
	// service account.
	Token TokenSource
	// HTTPClient carries every request. When nil an instrumented client with
	// no timeout is used: deadlines come from the caller's context, which is
	// the right knob for a held create and a long command. A Timeout set here
	// bounds each whole exchange, including a create held until the sandbox
	// runs and a command that runs for minutes.
	HTTPClient *http.Client
	// AllowedHosts are the hosts a created sandbox may reach, each an exact
	// name or one leading "*." wildcard. Nil admits api.latere.ai alone, the
	// Latere API origin, where the hosted Lux core serves the models an agent
	// in the sandbox calls; a non-nil slice admits exactly these hosts.
	AllowedHosts []string
}

// latereAPIHost is the one host a sandbox reaches when the caller names none:
// the platform origin that serves the hosted model gateway.
const latereAPIHost = "api.latere.ai"

// Provider implements [sandbox.Provider] against a Cella control plane's /v1
// API. It is safe for concurrent use.
type Provider struct {
	core         *cellaclient.Client
	allowedHosts []string
}

// New returns a Provider configured by opts. It panics if BaseURL or Token is
// empty, or if BaseURL is not an http or https address, since none of them has
// a sensible default and a misconfigured provider would fail every call.
//
// The exported client is always handed an http.Client. Its own transport
// allows ten seconds to the first response byte, and a create held until the
// sandbox runs, like a synchronous command, sends its first byte only when it
// is done.
func New(opts Options) *Provider {
	if opts.BaseURL == "" {
		panic("cella.New: BaseURL is required")
	}
	if opts.Token == nil {
		panic("cella.New: Token is required")
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = otel.HTTPClient()
	}
	core, err := cellaclient.New(cellaclient.Config{
		URL:        opts.BaseURL,
		Token:      cellaclient.TokenFunc(opts.Token.Token),
		HTTPClient: hc,
		UserAgent:  userAgent,
	})
	if err != nil {
		panic("cella.New: " + err.Error())
	}
	hosts := []string{latereAPIHost}
	if opts.AllowedHosts != nil {
		hosts = append([]string(nil), opts.AllowedHosts...)
	}
	return &Provider{core: core, allowedHosts: hosts}
}

// mapError converts a failed call into the error contract the
// [sandbox.Provider] interface specifies: a 404 refusal is
// [sandbox.ErrNotFound], a 409 refusal is [sandbox.ErrConflict], any other
// refusal is a [*sandbox.APIError] carrying the control plane's code, message
// and request id. A failure that reached no status (an unreachable address, a
// token source that failed, a cancelled context) is returned wrapped, so
// errors.Is still finds its cause.
func mapError(err error) error {
	var refusal *cellaclient.Error
	if !errors.As(err, &refusal) {
		return fmt.Errorf("cella: %w", err)
	}
	switch refusal.Status {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", sandbox.ErrNotFound, refusal.Message)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", sandbox.ErrConflict, refusal.Message)
	default:
		return &sandbox.APIError{
			Status:    refusal.Status,
			Code:      refusal.Code,
			Message:   refusal.Message,
			RequestID: refusal.RequestID,
		}
	}
}
