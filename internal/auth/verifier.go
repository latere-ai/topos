// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package auth is toposd's identity (spec 006): the verifier that turns
// a bearer into a subject, the owner policy that decides when no
// authorizer is configured, and the guard every API action asks through.
// toposd decides nothing about a person; the guard asks and reads the
// answer.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/authkit/jwt"
	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/bearer"
)

// The error codes of spec 006, and not_found, which a denied read answers
// so a deny does not disclose that the object exists.
const (
	CodeUnauthenticated       = "unauthenticated"
	CodeForbidden             = "forbidden"
	CodeAuthorizerUnavailable = "authorizer_unavailable"
	CodeNotFound              = "not_found"
)

// Error is a refusal with its code; the API renders the code and the
// message. Reason is, on a forbidden, the authorizer's reason for the
// deny, which the API returns to the caller; it is empty when the
// authorizer gave none, gave one that is not a reason token, or when the
// guard withheld it (Guard.Disclose). Limits are the deny's limits
// object, passed on beside a reason that is returned, so a deny for a
// bound that resets says when it does (spec 048); nil without a reason or
// an object.
type Error struct {
	Code    string
	Message string
	Reason  string
	Limits  json.RawMessage
	Err     error
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func (e *Error) Unwrap() error { return e.Err }

func refuse(code string, err error, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Err: err}
}

// Code is the code of an *Error in err's chain, and "" for any other
// error.
func Code(err error) string {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code
	}
	return ""
}

// Caller is a verified bearer. Subject is the rendered subject every
// owner, sender and authorizer request carries; Claims are the token's
// claims verbatim, handed to the authorizer and read by nothing here.
type Caller struct {
	Subject string
	Issuer  string
	Sub     string
	Claims  map[string]any
}

// Options configures a Verifier.
type Options struct {
	// Issuers are TOPOS_OIDC_ISSUERS.
	Issuers []string
	// Audiences are TOPOS_OIDC_AUDIENCE; a token must carry one of them.
	Audiences []string
	// LocalIssuer and LocalKeys are TOPOS_PUBLIC_URL and the public half
	// of TOPOS_LOCAL_ISSUER_KEY, set when toposd is also an issuer.
	LocalIssuer string
	LocalKeys   []jwt.LocalKey
	// HTTP reads the issuers' discovery documents and key sets.
	HTTP *http.Client
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// Verifier verifies bearers against the listed issuers and the local
// issuer.
type Verifier struct {
	issuers   []string
	local     string
	validator *jwt.Validator
}

// NewVerifier builds the verifier and reads every listed issuer's key
// set once. An issuer that does not answer, or whose discovery document
// names another issuer, stops the start: a wrong issuer is a deployment
// to fix, not a stream of refusals to read in a log.
func NewVerifier(ctx context.Context, o Options) (*Verifier, error) {
	v := &Verifier{local: strings.TrimRight(o.LocalIssuer, "/")}
	for _, raw := range o.Issuers {
		iss := strings.TrimRight(raw, "/")
		if slices.Contains(v.issuers, iss) || iss == v.local {
			return nil, fmt.Errorf("TOPOS_OIDC_ISSUERS lists %s twice", iss)
		}
		v.issuers = append(v.issuers, iss)
	}
	switch {
	case len(v.issuers) == 0 && len(o.LocalKeys) == 0:
		return nil, errors.New("TOPOS_OIDC_ISSUERS names no issuer and TOPOS_LOCAL_ISSUER_KEY is unset, so no token could verify")
	case len(o.LocalKeys) > 0 && v.local == "":
		return nil, errors.New("TOPOS_LOCAL_ISSUER_KEY is set while TOPOS_PUBLIC_URL is unset, so the local issuer has no name")
	case len(o.Audiences) == 0:
		return nil, errors.New("TOPOS_OIDC_AUDIENCE names no audience")
	}
	cfg := jwt.Config{
		Issuers:    v.issuers,
		Audiences:  o.Audiences,
		HTTPClient: o.HTTP,
		// The family's age bound: a token whose iat is older than a day
		// is refused whatever its exp, and a token with no iat has no
		// age to hold to the bound.
		MaxTokenAge:     jwt.DefaultMaxTokenAge,
		RequireIssuedAt: true,
		// A narrowed key's grants ride on its token, and the guard
		// applies them: the owner policy through authz.Restrict, an
		// installation's endpoint on authz/server by the same function.
		ReadsGrants: true,
		Now:         o.Now,
	}
	if len(o.LocalKeys) > 0 {
		cfg.LocalIssuer, cfg.LocalKeys = v.local, o.LocalKeys
	}
	v.validator = jwt.New(cfg)
	if err := v.validator.Warm(ctx); err != nil {
		return nil, fmt.Errorf("TOPOS_OIDC_ISSUERS: %w", err)
	}
	return v, nil
}

// Authenticator is the validator behind the family's
// authkit.Authenticator, the shape authkit's conformance suite drives.
func (v *Verifier) Authenticator() *jwt.Authenticator { return jwt.NewAuthenticator(v.validator) }

// Authenticate reads a request's bearer and verifies it. There is no
// anonymous access.
func (v *Verifier) Authenticate(r *http.Request) (Caller, error) {
	raw, ok := bearer.FromRequest(r)
	if !ok || raw == "" {
		return Caller{}, refuse(CodeUnauthenticated, nil, "the request carries no bearer token")
	}
	return v.Verify(raw)
}

// Verify verifies one bearer. The token's own iss selects the keys it is
// checked against, so a token of an unlisted issuer is refused before a
// signature is tried.
func (v *Verifier) Verify(raw string) (Caller, error) {
	var claims map[string]any
	if err := jwt.DecodePayload(raw, &claims); err != nil {
		return Caller{}, refuse(CodeUnauthenticated, err, "the bearer is not a token any issuer signed")
	}
	iss, _ := claims["iss"].(string)
	iss = strings.TrimRight(iss, "/")
	if !slices.Contains(v.issuers, iss) && (v.local == "" || iss != v.local) {
		return Caller{}, refuse(CodeUnauthenticated, nil, "the token names the issuer %s, which toposd does not accept", strconv.Quote(iss))
	}
	c, err := v.validator.Validate(raw)
	if err != nil {
		return Caller{}, refuse(CodeUnauthenticated, err, "issuer %s: %s", iss, jwt.ReasonOf(err))
	}
	return Caller{Subject: authz.Subject(iss, c.Sub), Issuer: iss, Sub: c.Sub, Claims: claims}, nil
}
