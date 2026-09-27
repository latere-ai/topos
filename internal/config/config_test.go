// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"maps"
	"slices"
	"strings"
	"testing"
)

func env(m map[string]string) Getenv {
	return func(k string) string { return m[k] }
}

// serve is m over the two variables serve requires.
func serve(m map[string]string) Getenv {
	all := map[string]string{"TOPOS_PUBLIC_URL": "https://topos.example", "TOPOS_OIDC_ISSUERS": "https://login.example", "TOPOS_DATA_DIR": "/var/lib/topos", "TOPOS_MODELS_URL": "https://lux.example/anthropic"}
	maps.Copy(all, m)
	return env(all)
}

func TestLoadAppliesDefaults(t *testing.T) {
	c, err := Load(RoleServe, serve(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicAddr != DefaultPublicAddr || c.InternalAddr != DefaultInternalAddr {
		t.Fatalf("got %+v, want the defaults", c)
	}
}

func TestLoadReadsTheVariables(t *testing.T) {
	c, err := Load(RoleServe, serve(map[string]string{
		"TOPOS_PUBLIC_ADDR":   "127.0.0.1:9080",
		"TOPOS_INTERNAL_ADDR": "127.0.0.1:9081",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicAddr != "127.0.0.1:9080" || c.InternalAddr != "127.0.0.1:9081" {
		t.Fatalf("got %+v", c)
	}
}

func TestBlankIsTheDefault(t *testing.T) {
	c, err := Load(RoleServe, serve(map[string]string{"TOPOS_PUBLIC_ADDR": "  ", "TOPOS_INTERNAL_ADDR": "\t"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicAddr != DefaultPublicAddr || c.InternalAddr != DefaultInternalAddr {
		t.Fatalf("PublicAddr = %q, InternalAddr = %q, want the defaults", c.PublicAddr, c.InternalAddr)
	}
}

func TestEveryProblemIsReportedAtOnceAndSorted(t *testing.T) {
	_, err := Load(RoleServe, serve(map[string]string{
		"TOPOS_PUBLIC_ADDR":   "nope",
		"TOPOS_INTERNAL_ADDR": "also-nope",
	}))
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	i, j := strings.Index(msg, "TOPOS_INTERNAL_ADDR"), strings.Index(msg, "TOPOS_PUBLIC_ADDR")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("message %q does not name both variables in sorted order", msg)
	}
	if !strings.HasPrefix(msg, "configuration: ") {
		t.Fatalf("message %q lacks the prefix", msg)
	}
}

func TestTheTwoListenersMustDiffer(t *testing.T) {
	_, err := Load(RoleServe, serve(map[string]string{
		"TOPOS_PUBLIC_ADDR":   ":9000",
		"TOPOS_INTERNAL_ADDR": ":9000",
	}))
	if err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("err = %v, want the two listeners refused", err)
	}
}

func TestPortZeroTwiceIsTwoSockets(t *testing.T) {
	if _, err := Load(RoleServe, serve(map[string]string{
		"TOPOS_PUBLIC_ADDR":   "127.0.0.1:0",
		"TOPOS_INTERNAL_ADDR": "127.0.0.1:0",
	})); err != nil {
		t.Fatalf("port 0 twice refused: %v", err)
	}
}

func TestTheDatabaseURLs(t *testing.T) {
	c, err := Load(RoleServe, serve(map[string]string{"TOPOS_DB_URL": "postgres://u@db/topos", "TOPOS_DB_POOL_URL": " postgres://u@pool/topos "}))
	if err != nil || c.DBURL != "postgres://u@db/topos" || c.DBPoolURL != "postgres://u@pool/topos" {
		t.Fatalf("config %+v, %v", c, err)
	}
	for name, vars := range map[string]map[string]string{
		"pool without direct": {"TOPOS_DB_POOL_URL": "postgres://u@pool/topos"},
		"not postgres":        {"TOPOS_DB_URL": "mysql://u@db/topos"},
	} {
		if _, err := Load(RoleServe, serve(vars)); err == nil {
			t.Fatalf("%s: loaded", name)
		}
	}
}

func pemOf(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestTheIdentityVariables(t *testing.T) {
	c, err := Load(RoleServe, env(map[string]string{
		"TOPOS_PUBLIC_URL":            "https://topos.example/",
		"TOPOS_OIDC_ISSUERS":          "https://login.example/, http://127.0.0.1:9000,http://issuer.test",
		"TOPOS_OIDC_INSECURE_ISSUERS": "http://issuer.test",
		"TOPOS_OIDC_AUDIENCE":         "topos, topos-cli",
		"TOPOS_AUTHORIZER_URL":        "https://authz.example/v1/authorize",
		"TOPOS_AUTHORIZER_TOKEN":      "secret",
		"TOPOS_ADMIN_SUBJECTS":        "https://login.example|root, ",
		"HOME":                        "/home/topos",
		"TOPOS_MODELS_URL":            "https://lux.example/anthropic",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "https://topos.example" || !slices.Equal(c.OIDCIssuers, []string{"https://login.example", "http://127.0.0.1:9000", "http://issuer.test"}) ||
		!slices.Equal(c.OIDCAudiences, []string{"topos", "topos-cli"}) || c.AuthorizerToken != "secret" || !slices.Equal(c.AdminSubjects, []string{"https://login.example|root"}) {
		t.Fatalf("config %+v", c)
	}
	if c, err := Load(RoleServe, serve(nil)); err != nil || !slices.Equal(c.OIDCAudiences, []string{DefaultAudience}) {
		t.Fatalf("the default audience: %+v, %v", c.OIDCAudiences, err)
	}
}

func TestServeRequiresAnIssuerAndThePublicURL(t *testing.T) {
	_, err := Load(RoleServe, env(nil))
	if err == nil || !strings.Contains(err.Error(), "TOPOS_PUBLIC_URL is required") || !strings.Contains(err.Error(), "TOPOS_OIDC_ISSUERS is required") {
		t.Fatalf("serve with no identity: %v", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(RoleServe, env(map[string]string{"TOPOS_PUBLIC_URL": "http://localhost:8080", "TOPOS_LOCAL_ISSUER_KEY": pemOf(t, key), "HOME": "/home/topos", "TOPOS_MODELS_URL": "https://lux.example/anthropic"})); err != nil {
		t.Fatalf("the local issuer alone: %v", err)
	}
	if _, err := Load(RoleRunner, env(map[string]string{"TOPOS_INTERNAL_URL": "http://toposd:8081", "TOPOS_RUNNER_TOKEN": "t", "TOPOS_MODELS_URL": "https://lux.example/anthropic"})); err != nil {
		t.Fatalf("the runner reads no identity variable: %v", err)
	}
}

func TestTheIdentityProblems(t *testing.T) {
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	for name, vars := range map[string]map[string]string{
		"relative public url":   {"TOPOS_PUBLIC_URL": "/topos"},
		"issuer with a query":   {"TOPOS_OIDC_ISSUERS": "https://login.example?x=1"},
		"plain http issuer":     {"TOPOS_OIDC_ISSUERS": "http://login.example"},
		"insecure but unlisted": {"TOPOS_OIDC_INSECURE_ISSUERS": "http://other.example"},
		"authorizer, no token":  {"TOPOS_AUTHORIZER_URL": "https://authz.example"},
		"authorizer not a url":  {"TOPOS_AUTHORIZER_URL": "authz", "TOPOS_AUTHORIZER_TOKEN": "t"},
		"a key that is not pem": {"TOPOS_LOCAL_ISSUER_KEY": "not a key"},
		"an rsa key too small":  {"TOPOS_LOCAL_ISSUER_KEY": pemOf(t, small)},
	} {
		if _, err := Load(RoleServe, serve(vars)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

func TestTheTokenRoleReadsItsThreeVariables(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Load(RoleToken, env(map[string]string{
		"TOPOS_PUBLIC_URL": "https://topos.example", "TOPOS_LOCAL_ISSUER_KEY": pemOf(t, key),
		"TOPOS_PUBLIC_ADDR": "not read by the token role",
	}))
	if err != nil || c.LocalIssuerKey == "" || c.OIDCAudiences[0] != DefaultAudience {
		t.Fatalf("token role: %+v, %v", c, err)
	}
	_, err = Load(RoleToken, env(nil))
	if err == nil || !strings.Contains(err.Error(), "TOPOS_LOCAL_ISSUER_KEY is required") || !strings.Contains(err.Error(), "TOPOS_PUBLIC_URL is required") {
		t.Fatalf("token role with nothing: %v", err)
	}
}

func TestDataDir(t *testing.T) {
	for env, want := range map[string]string{"TOPOS_DATA_DIR=/d": "/d", "XDG_STATE_HOME=/x": "/x/topos", "HOME=/h": "/h/.local/state/topos"} {
		k, v, _ := strings.Cut(env, "=")
		got, err := DataDir(env2(k, v))
		if err != nil || got != want {
			t.Fatalf("DataDir with %s = %q, %v", env, got, err)
		}
	}
	if _, err := DataDir(env(nil)); err == nil {
		t.Fatal("a data directory from nothing")
	}
	c, err := Load(RoleServe, serve(nil))
	if err != nil || c.DataDir != "/var/lib/topos" {
		t.Fatalf("serve's data directory: %q, %v", c.DataDir, err)
	}
	if _, err := Load(RoleServe, env(map[string]string{"TOPOS_PUBLIC_URL": "https://t.example", "TOPOS_OIDC_ISSUERS": "https://l.example"})); err == nil || !strings.Contains(err.Error(), "TOPOS_DATA_DIR") {
		t.Fatalf("serve with no data directory: %v", err)
	}
}

func env2(k, v string) Getenv { return env(map[string]string{k: v}) }

func TestTheRunnerVariables(t *testing.T) {
	c, err := Load(RoleServe, serve(map[string]string{
		"TOPOS_MODELS_URL": "https://lux.example/anthropic/", "TOPOS_MODELS_KEY": " k ", "TOPOS_RUNNER_CAPACITY": "3",
		"TOPOS_CELLA_URL": "https://cella.example/v1/environments", "TOPOS_CELLA_TOKEN_FILE": "/run/cella/token",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ModelsURL != "https://lux.example/anthropic" || c.ModelsKey != "k" || c.RunnerCapacity != 3 || c.CellaURL != "https://cella.example/v1/environments" ||
		c.CellaTokenFile != "/run/cella/token" || c.MachineHelpers != DefaultMachineHelpers {
		t.Fatalf("config %+v", c)
	}
	if c, err := Load(RoleServe, serve(nil)); err != nil || c.RunnerCapacity != DefaultRunnerCapacity || c.CellaURL != "" {
		t.Fatalf("the defaults: %+v, %v", c, err)
	}
	for name, vars := range map[string]map[string]string{
		"no models url":        {"TOPOS_MODELS_URL": " "},
		"a scripted model":     {"TOPOS_MODELS_URL": "scripted:/tmp/s.yaml"},
		"models url not a url": {"TOPOS_MODELS_URL": "lux"},
		"capacity":             {"TOPOS_RUNNER_CAPACITY": "-1"},
		"cella url":            {"TOPOS_CELLA_URL": "cella", "TOPOS_CELLA_TOKEN_FILE": "/t"},
		"cella without token":  {"TOPOS_CELLA_URL": "https://cella.example"},
		"relative machine dir": {"TOPOS_MACHINE_DIR": "tmp/topos"},
	} {
		if _, err := Load(RoleServe, serve(vars)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	runnerVars := map[string]string{"TOPOS_INTERNAL_URL": "http://toposd:8081/", "TOPOS_RUNNER_TOKEN": "new, old", "TOPOS_MODELS_URL": "https://lux.example/anthropic"}
	r, err := Load(RoleRunner, env(runnerVars))
	if err != nil || r.InternalURL != "http://toposd:8081" || !slices.Equal(r.RunnerTokens, []string{"new", "old"}) || r.RunnerCapacity != DefaultRunnerCapacity || r.PublicURL != "" {
		t.Fatalf("the runner role %+v, %v", r, err)
	}
	for name, mut := range map[string]map[string]string{
		"no internal url":  {"TOPOS_INTERNAL_URL": ""},
		"bad internal url": {"TOPOS_INTERNAL_URL": "toposd"},
		"no token":         {"TOPOS_RUNNER_TOKEN": ""},
		"no capacity":      {"TOPOS_RUNNER_CAPACITY": "0"},
		"no models":        {"TOPOS_MODELS_URL": ""},
	} {
		vars := maps.Clone(runnerVars)
		maps.Copy(vars, mut)
		if _, err := Load(RoleRunner, env(vars)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	if s, err := Load(RoleServe, serve(map[string]string{"TOPOS_RUNNER_TOKEN": "t"})); err != nil || !slices.Equal(s.RunnerTokens, []string{"t"}) {
		t.Fatalf("serve's runner tokens: %+v, %v", s.RunnerTokens, err)
	}
}
