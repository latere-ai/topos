// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"latere.ai/x/topos/test/stubs/idpstub"
)

func newClient(t *testing.T) (*Client, *idpstub.Server, string) {
	t.Helper()
	idp := idpstub.New(t, "topos-host", "s3cret")
	file := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(file, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{URL: idp.URL() + "/", ClientID: "topos-host", SecretFile: file, HTTP: http.DefaultClient})
	if err != nil {
		t.Fatal(err)
	}
	return c, idp, file
}

func code(err error) string {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code
	}
	return ""
}

// TestTheHostsCalls: the host creates an identity once per agent and is
// answered the same subject again, archives and disables it with the
// confirmation, lists the archived ones, and mints a token that carries
// the agent's subject, one audience and the session, all with one host
// token.
func TestTheHostsCalls(t *testing.T) {
	c, idp, _ := newClient(t)
	ctx := t.Context()
	owner := Owner{Type: OwnerUser, ID: "alice"}
	sub, err := c.Create(ctx, "agent_01", "reviewer", owner, "alice")
	if err != nil || sub == "" {
		t.Fatalf("create: %q %v", sub, err)
	}
	if again, err := c.Create(ctx, "agent_01", "reviewer", owner, "alice"); err != nil || again != sub {
		t.Fatalf("a repeated create answered %q %v", again, err)
	}
	if _, err := c.Create(ctx, "agent_01", "reviewer", Owner{Type: OwnerOrganization, ID: "org"}, "alice"); code(err) != "owner_mismatch" {
		t.Fatalf("another owner: %v", err)
	}
	tok, err := c.Mint(ctx, sub, "cella", Session{ID: "ses_01", Workload: WorkloadSandbox})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := idp.Token(tok.Value)
	if !ok || m.Subject != sub || m.Audience != "cella" || m.SessionID != "ses_01" || m.Workload != WorkloadSandbox {
		t.Fatalf("minted %+v %v", m, ok)
	}
	if left := time.Until(tok.ExpiresAt); left <= 14*time.Minute || left > 15*time.Minute {
		t.Fatalf("the token expires in %s", left)
	}
	if _, err := c.Mint(ctx, sub, "lux", Session{ID: "ses_01", Workload: WorkloadSession}); code(err) != "invalid_target" {
		t.Fatalf("an audience the host may not mint for: %v", err)
	}
	if err := c.Archive(ctx, "agent_01"); err != nil {
		t.Fatal(err)
	}
	archived, err := c.Archived(ctx)
	if err != nil || len(archived) != 1 || archived[0].Subject != sub || archived[0].Ref != "agent_01" {
		t.Fatalf("archived %+v %v", archived, err)
	}
	if err := c.Disable(ctx, "agent_01", sub); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Mint(ctx, sub, "cella", Session{ID: "ses_01", Workload: WorkloadSession}); code(err) != "agent_disabled" {
		t.Fatalf("a disabled agent minted: %v", err)
	}
	if archived, err := c.Archived(ctx); err != nil || len(archived) != 0 {
		t.Fatalf("archived after the disable %+v %v", archived, err)
	}
	if n := idp.Count(idpstub.OpToken); n != 1 {
		t.Fatalf("%d host tokens fetched for one lifetime", n)
	}
	// An identity the provider deleted with its owner is unknown to
	// archive and disable alike, and no other refusal reads as unknown.
	if _, err := c.Create(ctx, "agent_02", "helper", owner, "alice"); err != nil {
		t.Fatal(err)
	}
	if n := idp.DeleteOwner(idpstub.Owner(owner)); n != 2 {
		t.Fatalf("the owner's deletion removed %d identities", n)
	}
	if err := c.Archive(ctx, "agent_02"); !Unknown(err) {
		t.Fatalf("an archive of a deleted identity: %v", err)
	}
	if err := c.Disable(ctx, "agent_01", sub); !Unknown(err) {
		t.Fatalf("a disable of a deleted identity: %v", err)
	}
	if Unknown(ErrUnavailable) || Unknown(&Error{Status: http.StatusBadRequest, Code: CodeUnknownAgent}) || Unknown(&Error{Status: http.StatusNotFound, Code: "not_found"}) {
		t.Fatal("another error reads as an unknown identity")
	}
	var e *Error
	if _, err := c.Create(ctx, "agent_01", "", owner, ""); !errors.As(err, &e) || e.Status != http.StatusBadRequest || e.Error() == "" {
		t.Fatalf("a nameless create: %v", err)
	}
}

// TestTheHostTokenIsHeldUntilTwoMinutesBeforeItsExpiry: a host token
// with less than two minutes left is fetched again, and a secret
// rotated in its file is read at that fetch; a token the provider
// refuses is dropped.
func TestTheHostTokenIsHeldUntilTwoMinutesBeforeItsExpiry(t *testing.T) {
	c, idp, file := newClient(t)
	fetched := func() int { return idp.Count(idpstub.OpToken) }
	for range 2 {
		if _, err := c.Archived(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if fetched() != 1 {
		t.Fatalf("a fresh host token was not held: %d fetches", fetched())
	}
	idp.Fail(idpstub.OpList, idpstub.Failure{Status: http.StatusUnauthorized, Code: "unauthorized"})
	if _, err := c.Archived(t.Context()); code(err) != "unauthorized" {
		t.Fatalf("a refused host token: %v", err)
	}
	if _, err := c.Archived(t.Context()); err != nil || fetched() != 2 {
		t.Fatalf("the refused host token was kept: %d %v", fetched(), err)
	}
	// A token with less than two minutes of life is fetched again at
	// each call, reading the secret's file each time.
	idp.SetHostTokenLifetime(90 * time.Second)
	c.forget()
	if _, err := c.Archived(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Archived(t.Context()); !errors.Is(err, ErrUnavailable) || fetched() != 4 {
		t.Fatalf("a token within two minutes of expiry was kept, or the rotated secret was not read: %d %v", fetched(), err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Archived(t.Context()); err == nil {
		t.Fatal("a missing secret file fetched a token")
	}
}

// TestAnUnansweredProviderIsUnavailable: a provider that does not answer
// or answers a server error is ErrUnavailable, and an answer that is not
// the contract's is an error.
func TestAnUnansweredProviderIsUnavailable(t *testing.T) {
	c, idp, _ := newClient(t)
	idp.Fail(idpstub.OpPut, idpstub.Failure{Status: http.StatusBadGateway, Code: "bad_gateway"})
	if _, err := c.Create(t.Context(), "agent_01", "a", Owner{Type: OwnerUser, ID: "u"}, ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a 502: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Archive(ctx, "agent_01"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a canceled call: %v", err)
	}
	if _, err := New(Options{URL: "x"}); err == nil {
		t.Fatal("built without a client id")
	}
	if _, err := New(Options{URL: "x", ClientID: "c", SecretFile: "f"}); err == nil {
		t.Fatal("built without an HTTP client")
	}
}

// TestServiceTokenAsksTheAudience: the host's own token for another
// service is the client credentials grant asked for that audience, held
// while it lives, apart from the host's token at the provider; an empty
// audience and a provider that refuses are errors (spec 068).
func TestServiceTokenAsksTheAudience(t *testing.T) {
	c, idp, file := newClient(t)
	ctx := t.Context()
	tok, err := c.ServiceToken(ctx, "arca")
	if err != nil {
		t.Fatal(err)
	}
	if aud, ok := idp.ServiceToken(tok.Value); !ok || aud != "arca" || tok.ExpiresAt.IsZero() {
		t.Fatalf("the token %+v was asked for %q %t", tok, aud, ok)
	}
	if again, err := c.ServiceToken(ctx, "arca"); err != nil || again != tok || idp.Count(idpstub.OpToken) != 1 {
		t.Fatalf("a live token was not held: %v, %d fetches", err, idp.Count(idpstub.OpToken))
	}
	if _, err := c.Archived(ctx); err != nil || idp.Count(idpstub.OpToken) != 2 {
		t.Fatalf("the provider's own routes took the service token: %v", err)
	}
	if _, err := c.ServiceToken(ctx, ""); err == nil {
		t.Fatal("a token for no audience")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ServiceToken(ctx, "other"); err == nil {
		t.Fatal("a token with no secret")
	}
	if err := os.WriteFile(file, []byte("wrong"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ServiceToken(ctx, "other"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a refused grant: %v", err)
	}
}
