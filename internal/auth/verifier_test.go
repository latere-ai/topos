// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit"
	authkitconformance "latere.ai/x/pkg/authkit/conformance"
	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authkit/jwt"

	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/token"
)

const audience = "topos"

// TestServiceConformance is the family's rule for a verifier: toposd
// checks that the audience is its own, reads one identity, and calls the
// issuer for nothing but its discovery document and key set.
func TestServiceConformance(t *testing.T) {
	authkitconformance.Run(t, authkitconformance.Service{
		Audience: audience,
		New: func(tb testing.TB, issuerURL, _ string) authkit.Authenticator {
			tb.Helper()
			v, err := auth.NewVerifier(t.Context(), auth.Options{Issuers: []string{issuerURL}, Audiences: []string{audience}})
			if err != nil {
				tb.Fatalf("the verifier toposd runs would not build: %v", err)
			}
			return v.Authenticator()
		},
	})
}

func signer(t *testing.T, iss string) *token.Signer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	s, err := token.New(iss, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func code(t *testing.T, err error) string {
	t.Helper()
	var e *auth.Error
	if !errors.As(err, &e) {
		t.Fatalf("%v is not an auth.Error", err)
	}
	if auth.Code(err) != e.Code || !strings.Contains(err.Error(), e.Code) {
		t.Fatalf("Code(%v) = %q", err, auth.Code(err))
	}
	return e.Code
}

// TestVerificationRules: a token older than the age bound, with no iat,
// with a wrong audience, badly signed, or from an unlisted issuer is
// refused unauthenticated; a good one is the subject <iss>|<sub>.
func TestVerificationRules(t *testing.T) {
	listed := issuertest.New(t, issuertest.WithDefaultAudience(audience))
	unlisted := issuertest.New(t, issuertest.WithDefaultAudience(audience))
	local := signer(t, "https://topos.example")
	v, err := auth.NewVerifier(t.Context(), auth.Options{
		Issuers: []string{listed.URL() + "/"}, Audiences: []string{audience, "topos-cli"},
		LocalIssuer: local.Issuer(), LocalKeys: []jwt.LocalKey{local.LocalKey()},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	good := listed.Mint(issuertest.Claims{Sub: "alice", Iat: now.Unix()})
	c, err := v.Verify(good)
	if err != nil || c.Subject != listed.URL()+"|alice" || c.Sub != "alice" || c.Issuer != listed.URL() || c.Claims["sub"] != "alice" {
		t.Fatalf("a good token: %+v, %v", c, err)
	}
	second, err := v.Verify(listed.Mint(issuertest.Claims{Sub: "bob", Iat: now.Unix(), Aud: issuertest.StringList{"topos-cli"}}))
	if err != nil || second.Sub != "bob" {
		t.Fatalf("the second audience: %+v, %v", second, err)
	}
	minted, err := local.Sign("admin", audience, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := v.Verify(minted); err != nil || c.Subject != "https://topos.example|admin" {
		t.Fatalf("a local token: %+v, %v", c, err)
	}
	foreign, err := signer(t, "https://topos.example").Sign("admin", audience, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	wrongAud, err := local.Sign("admin", "lux", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"too old":           listed.Mint(issuertest.Claims{Iat: now.Add(-25 * time.Hour).Unix(), Exp: now.Add(time.Hour).Unix()}),
		"no iat":            listed.Mint(issuertest.Claims{Omit: []string{"iat"}}),
		"wrong audience":    listed.Mint(issuertest.Claims{Iat: now.Unix(), Aud: issuertest.StringList{"lux"}}),
		"expired":           listed.Mint(issuertest.Claims{Iat: now.Add(-2 * time.Hour).Unix(), Exp: now.Add(-time.Hour).Unix()}),
		"unlisted issuer":   unlisted.Mint(issuertest.Claims{Iat: now.Unix()}),
		"bad signature":     good[:len(good)-4] + "AAAA",
		"not a jws":         "garbage",
		"another local key": foreign,
		"local, wrong aud":  wrongAud,
	} {
		if _, err := v.Verify(raw); code(t, err) != auth.CodeUnauthenticated {
			t.Errorf("%s: %v", name, err)
		}
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	if _, err := v.Authenticate(r); code(t, err) != auth.CodeUnauthenticated {
		t.Fatalf("no bearer: %v", err)
	}
	r.Header.Set("Authorization", "Bearer "+good)
	if c, err := v.Authenticate(r); err != nil || c.Sub != "alice" {
		t.Fatalf("Authenticate: %+v, %v", c, err)
	}
}

func TestNewVerifierRefuses(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	iss := issuertest.New(t)
	local := signer(t, "https://topos.example")
	for name, tc := range map[string]struct {
		o    auth.Options
		want string
	}{
		"nothing to verify": {auth.Options{Audiences: []string{audience}}, "no issuer"},
		"key without name":  {auth.Options{Audiences: []string{audience}, LocalKeys: []jwt.LocalKey{local.LocalKey()}}, "TOPOS_PUBLIC_URL"},
		"no audience":       {auth.Options{Issuers: []string{iss.URL()}}, "TOPOS_OIDC_AUDIENCE"},
		"twice":             {auth.Options{Issuers: []string{iss.URL(), iss.URL() + "/"}, Audiences: []string{audience}}, "twice"},
		"unreachable":       {auth.Options{Issuers: []string{gone.URL}, Audiences: []string{audience}}, "TOPOS_OIDC_ISSUERS"},
	} {
		if _, err := auth.NewVerifier(t.Context(), tc.o); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want it to name %q", name, err, tc.want)
		}
	}
}
