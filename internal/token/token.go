// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package token is toposd's local issuer (spec 006): the key of
// TOPOS_LOCAL_ISSUER_KEY, the tokens `toposd token` signs with it as
// TOPOS_PUBLIC_URL, and the key set toposd publishes so the same tokens
// verify anywhere. It signs and publishes; verifying is the shared
// validator's, which holds the public half as a local key.
package token

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"strings"
	"time"

	"latere.ai/x/pkg/authkit/jwt"
)

// MaxTTL is the longest lifetime `toposd token` signs, the same bound the
// validator's age rule holds every token to.
const MaxTTL = 24 * time.Hour

// JWKSPath is where toposd serves the key set, under the path of
// TOPOS_PUBLIC_URL.
const JWKSPath = "/.well-known/jwks.json"

// minRSABits is the smallest RSA key spec 002 accepts.
const minRSABits = 2048

// Signer signs tokens as the local issuer.
type Signer struct {
	issuer string
	key    crypto.Signer
	alg    string
	kid    string
	jwks   []byte
	now    func() time.Time
}

// New builds the signer from the PEM PKCS#8 private key of
// TOPOS_LOCAL_ISSUER_KEY, an ECDSA P-256 key signing ES256 or an RSA key
// of at least 2048 bits signing RS256. issuer is TOPOS_PUBLIC_URL; now
// is the clock, time.Now when nil.
func New(issuer, keyPEM string, now func() time.Time) (*Signer, error) {
	issuer = strings.TrimRight(issuer, "/")
	if issuer == "" {
		return nil, errors.New("TOPOS_PUBLIC_URL is unset, and it is the iss of every token the local issuer signs")
	}
	key, err := parseKey(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("TOPOS_LOCAL_ISSUER_KEY: %w", err)
	}
	if now == nil {
		now = time.Now
	}
	s := &Signer{issuer: issuer, key: key, now: now}
	var public map[string]string
	switch k := key.(type) {
	case *ecdsa.PrivateKey:
		s.alg = "ES256"
		public = map[string]string{"kty": "EC", "crv": "P-256", "x": b64(pad(k.X, 32)), "y": b64(pad(k.Y, 32))}
	case *rsa.PrivateKey:
		s.alg = "RS256"
		public = map[string]string{"kty": "RSA", "n": b64(k.N.Bytes()), "e": b64(big.NewInt(int64(k.E)).Bytes())}
	}
	s.kid = thumbprint(public)
	entry := map[string]string{"use": "sig", "alg": s.alg, "kid": s.kid}
	maps.Copy(entry, public)
	s.jwks = mustJSON(map[string]any{"keys": []map[string]string{entry}})
	return s, nil
}

// parseKey reads one PEM PKCS#8 private key of the two accepted shapes.
func parseKey(text string) (crypto.Signer, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(text)))
	if block == nil {
		return nil, errors.New("holds no PEM block")
	}
	if block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("holds a %q block; a PKCS#8 \"PRIVATE KEY\" is expected", block.Type)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	switch k := parsed.(type) {
	case *ecdsa.PrivateKey:
		if k.Curve != elliptic.P256() {
			return nil, fmt.Errorf("is an ECDSA key on %s; P-256 is the one curve ES256 signs with", k.Curve.Params().Name)
		}
		return k, nil
	case *rsa.PrivateKey:
		if k.N.BitLen() < minRSABits {
			return nil, fmt.Errorf("is an RSA key of %d bits; at least %d are required", k.N.BitLen(), minRSABits)
		}
		return k, nil
	}
	return nil, fmt.Errorf("is a %T; an ECDSA P-256 or an RSA key is expected", parsed)
}

// Issuer is the iss of every token the signer signs.
func (s *Signer) Issuer() string { return s.issuer }

// KeyID is the kid every token names: the RFC 7638 thumbprint of the
// public key.
func (s *Signer) KeyID() string { return s.kid }

// LocalKey is the public half as the shared validator holds it.
func (s *Signer) LocalKey() jwt.LocalKey {
	return jwt.LocalKey{KeyID: s.kid, Key: s.key.Public()}
}

// JWKS is the key set document served at JWKSPath.
func (s *Signer) JWKS() []byte { return s.jwks }

// Sign signs one token for subject, addressed to audience, living ttl.
// A ttl of zero or above MaxTTL is refused.
func (s *Signer) Sign(subject, audience string, ttl time.Duration) (string, error) {
	switch {
	case subject == "":
		return "", errors.New("the subject is empty")
	case strings.Contains(subject, "|"):
		return "", fmt.Errorf("the subject %q holds a vertical bar, which separates the issuer from the sub", subject)
	case audience == "":
		return "", errors.New("the audience is empty")
	case ttl <= 0 || ttl > MaxTTL:
		return "", fmt.Errorf("the lifetime %s is not between zero and %s", ttl, MaxTTL)
	}
	now := s.now().UTC().Truncate(time.Second)
	header := mustJSON(map[string]string{"alg": s.alg, "typ": "JWT", "kid": s.kid})
	claims := mustJSON(map[string]any{
		"iss": s.issuer, "sub": subject, "aud": audience,
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(ttl).Unix(),
		"jti": rand.Text(),
	})
	input := b64(header) + "." + b64(claims)
	sig, err := s.sign([]byte(input))
	if err != nil {
		return "", err
	}
	return input + "." + b64(sig), nil
}

// sign is the JWS signature over input: PKCS #1 v1.5 for RS256, and the
// fixed-width r and s of RFC 7518 section 3.4 for ES256, the one other
// key New accepts.
func (s *Signer) sign(input []byte) ([]byte, error) {
	sum := sha256.Sum256(input)
	if k, ok := s.key.(*rsa.PrivateKey); ok {
		return rsa.SignPKCS1v15(nil, k, crypto.SHA256, sum[:])
	}
	k, ok := s.key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("a %T key signs nothing", s.key)
	}
	r, sv, err := ecdsa.Sign(rand.Reader, k, sum[:])
	if err != nil {
		return nil, err
	}
	return append(pad(r, 32), pad(sv, 32)...), nil
}

// thumbprint is RFC 7638: the SHA-256 of the required members in
// lexicographic order, which encoding/json writes for a map.
func thumbprint(public map[string]string) string {
	sum := sha256.Sum256(mustJSON(public))
	return b64(sum[:])
}

// mustJSON encodes a value of fixed string and number members, which
// cannot fail.
func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func pad(n *big.Int, size int) []byte {
	return n.FillBytes(make([]byte, size))
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
