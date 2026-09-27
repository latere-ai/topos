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

func TestOwnerOfReadsTheOrganizationClaim(t *testing.T) {
	if o := OwnerOf("alice", map[string]any{"org_id": "org-1"}); o != (Owner{Type: OwnerOrganization, ID: "org-1"}) {
		t.Fatalf("an organization's token: %+v", o)
	}
	for _, claims := range []map[string]any{nil, {"org_id": ""}, {"org_id": 7}} {
		if o := OwnerOf("alice", claims); o != (Owner{Type: OwnerUser, ID: "alice"}) {
			t.Fatalf("a personal token %v: %+v", claims, o)
		}
	}
}
