// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package token

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/jwt"
)

const issuer = "https://topos.example"

func pemOf(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestSignedTokensVerifyUnderTheSharedValidator(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	for alg, key := range map[string]any{"ES256": ec, "RS256": rs} {
		s, err := New(issuer+"/", pemOf(t, key), clock)
		if err != nil {
			t.Fatal(err)
		}
		if s.Issuer() != issuer || s.LocalKey().KeyID != s.KeyID() {
			t.Fatalf("%s: issuer %q, kid %q", alg, s.Issuer(), s.KeyID())
		}
		raw, err := s.Sign("admin", "topos", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		h, err := jwt.ParseHeader(raw)
		if err != nil || h.Alg != alg || h.KID != s.KeyID() {
			t.Fatalf("%s: header %+v, %v", alg, h, err)
		}
		v := jwt.New(jwt.Config{LocalIssuer: issuer, LocalKeys: []jwt.LocalKey{s.LocalKey()}, Audiences: []string{"topos"}, RequireIssuedAt: true, Now: clock})
		c, err := v.Validate(raw)
		if err != nil || c.Sub != "admin" || !c.Exp.Equal(now.Add(time.Hour)) {
			t.Fatalf("%s: %+v, %v", alg, c, err)
		}
		late := jwt.New(jwt.Config{LocalIssuer: issuer, LocalKeys: []jwt.LocalKey{s.LocalKey()}, Audiences: []string{"topos"}, Now: func() time.Time { return now.Add(2 * time.Hour) }})
		if _, err := late.Validate(raw); err == nil {
			t.Fatalf("%s: an expired token verified", alg)
		}
		var set struct {
			Keys []map[string]string `json:"keys"`
		}
		if err := json.Unmarshal(s.JWKS(), &set); err != nil || len(set.Keys) != 1 || set.Keys[0]["kid"] != s.KeyID() || set.Keys[0]["alg"] != alg {
			t.Fatalf("%s: key set %s, %v", alg, s.JWKS(), err)
		}
	}
}

func TestTheThumbprintIsRFC7638(t *testing.T) {
	// RFC 7638 section 3.1's example key and its thumbprint.
	got := thumbprint(map[string]string{
		"kty": "RSA",
		"e":   "AQAB",
		"n":   "0vx7agoebGcQSuuPiLJXZptN9nndrQmbXEps2aiAFbWhM78LhWx4cbbfAAtVT86zwu1RK7aPFFxuhDR1L6tSoc_BJECPebWKRXjBZCiFV4n3oknjhMstn64tZ_2W-5JsGY4Hc5n9yBXArwl93lqt7_RN5w6Cf0h4QyQ5v-65YGjQR0_FDW2QvzqY368QQMicAtaSqzs8KJZgnYb9c7d0zgdAZHzu6qMQvRL5hajrn1n91CbOpbISD08qNLyrdkt-bFTWhAI4vMQFh6WeZu0fM4lFd2NcRwr3XPksINHaQ-G_xBniIqbw0Ls1jF44-csFCur-kEgU8awapJzKnqDKgw",
	})
	if got != "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs" {
		t.Fatalf("thumbprint = %s", got)
	}
}

func TestSignRefuses(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(issuer, pemOf(t, key), nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, try := range map[string]func() error{
		"empty subject":  func() error { _, err := s.Sign("", "topos", time.Hour); return err },
		"bar in subject": func() error { _, err := s.Sign("a|b", "topos", time.Hour); return err },
		"empty audience": func() error { _, err := s.Sign("admin", "", time.Hour); return err },
		"zero lifetime":  func() error { _, err := s.Sign("admin", "topos", 0); return err },
		"over a day":     func() error { _, err := s.Sign("admin", "topos", MaxTTL+time.Second); return err },
	} {
		if try() == nil {
			t.Errorf("%s: signed", name)
		}
	}
	if _, err := s.Sign("admin", "topos", MaxTTL); err != nil {
		t.Fatalf("a day: %v", err)
	}
}

func TestNewRefusesKeys(t *testing.T) {
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sec1, err := x509.MarshalECPrivateKey(p256)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ iss, key, want string }{
		"no issuer":   {"", pemOf(t, p256), "TOPOS_PUBLIC_URL"},
		"no pem":      {issuer, "key", "no PEM block"},
		"sec1 block":  {issuer, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1})), "PKCS#8"},
		"bad der":     {issuer, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")})), "TOPOS_LOCAL_ISSUER_KEY"},
		"p384":        {issuer, pemOf(t, p384), "P-384"},
		"small rsa":   {issuer, pemOf(t, small), "1024 bits"},
		"ed25519 key": {issuer, pemOf(t, ed), "ed25519"},
	} {
		if _, err := New(tc.iss, tc.key, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want it to name %q", name, err, tc.want)
		}
	}
}
