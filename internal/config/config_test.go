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

// TestHome: the home directory is HOME, or USERPROFILE, which Windows
// sets where HOME is unset, and HOME wins when both are set.
func TestHome(t *testing.T) {
	for vars, want := range map[string]string{"HOME=/h": "/h", "USERPROFILE=/u": "/u", "": ""} {
		k, v, _ := strings.Cut(vars, "=")
		if got := Home(env2(k, v)); got != want {
			t.Errorf("Home with %q = %q, want %q", vars, got, want)
		}
	}
	if got := Home(env(map[string]string{"HOME": "/h", "USERPROFILE": "/u"})); got != "/h" {
		t.Errorf("Home with both = %q", got)
	}
}

func TestDataDir(t *testing.T) {
	for env, want := range map[string]string{"TOPOS_DATA_DIR=/d": "/d", "XDG_STATE_HOME=/x": "/x/topos", "HOME=/h": "/h/.local/state/topos", "USERPROFILE=/u": "/u/.local/state/topos"} {
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

// TestHostSessions: TOPOS_HOST_SESSIONS is off unless it is on, any other
// value is a problem, and a runner role with it on reads the data
// directory its session directories live in.
func TestHostSessions(t *testing.T) {
	for v, want := range map[string]bool{"": false, "off": false, " on ": true} {
		c, err := Load(RoleServe, serve(map[string]string{"TOPOS_HOST_SESSIONS": v}))
		if err != nil || c.HostSessions != want {
			t.Fatalf("TOPOS_HOST_SESSIONS=%q: %v, %v", v, c.HostSessions, err)
		}
	}
	if _, err := Load(RoleServe, serve(map[string]string{"TOPOS_HOST_SESSIONS": "yes"})); err == nil || !strings.Contains(err.Error(), `TOPOS_HOST_SESSIONS is "yes", either on or off`) {
		t.Fatalf("a value that is neither: %v", err)
	}
	runnerVars := map[string]string{"TOPOS_INTERNAL_URL": "http://toposd:8081", "TOPOS_RUNNER_TOKEN": "t", "TOPOS_MODELS_URL": "https://lux.example/anthropic", "TOPOS_DATA_DIR": "/srv/topos"}
	r, err := Load(RoleRunner, env(runnerVars))
	if err != nil || r.HostSessions || r.DataDir != "" {
		t.Fatalf("a runner without host sessions: %+v, %v", r, err)
	}
	runnerVars["TOPOS_HOST_SESSIONS"] = "on"
	if r, err = Load(RoleRunner, env(runnerVars)); err != nil || !r.HostSessions || r.DataDir != "/srv/topos" {
		t.Fatalf("a runner with host sessions: %+v, %v", r, err)
	}
	delete(runnerVars, "TOPOS_DATA_DIR")
	if _, err := Load(RoleRunner, env(runnerVars)); err == nil || !strings.Contains(err.Error(), "TOPOS_DATA_DIR") {
		t.Fatalf("a runner with host sessions and no data directory: %v", err)
	}
}

func TestTheBlobVariables(t *testing.T) {
	c, err := Load(RoleServe, serve(map[string]string{"TOPOS_BLOB_URL": "s3://objects.example/bucket/topos", "TOPOS_BLOB_ACCESS_KEY": "k", "TOPOS_BLOB_SECRET_KEY": " s "}))
	if err != nil || c.BlobURL != "s3://objects.example/bucket/topos" || c.BlobAccessKey != "k" || c.BlobSecretKey != "s" {
		t.Fatalf("config %+v, %v", c, err)
	}
	if c, err := Load(RoleServe, serve(map[string]string{"TOPOS_BLOB_URL": "file:///var/lib/topos/blobs"})); err != nil || c.BlobURL != "file:///var/lib/topos/blobs" {
		t.Fatalf("a file store: %+v, %v", c, err)
	}
	if c, err := Load(RoleServe, serve(nil)); err != nil || c.BlobURL != "" {
		t.Fatalf("no blob store: %+v, %v", c, err)
	}
	for name, vars := range map[string]map[string]string{
		"s3 without keys":   {"TOPOS_BLOB_URL": "s3://objects.example/bucket"},
		"s3 without bucket": {"TOPOS_BLOB_URL": "s3://objects.example", "TOPOS_BLOB_ACCESS_KEY": "k", "TOPOS_BLOB_SECRET_KEY": "s"},
		"relative file":     {"TOPOS_BLOB_URL": "file:relative"},
		"another scheme":    {"TOPOS_BLOB_URL": "https://objects.example/bucket"},
		"not a url":         {"TOPOS_BLOB_URL": "%"},
	} {
		if _, err := Load(RoleServe, serve(vars)); err == nil || !strings.Contains(err.Error(), "TOPOS_BLOB_URL") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestTheCredentialVariables: the identity provider's and the session
// keys' variables are read; each URL needs its partners and the
// installation's authorizer, and neither installation credential is
// accepted beside the per-session one that replaces it; a server with an
// identity provider needs no Cella bearer file, and a runner role needs
// none either.
func TestTheCredentialVariables(t *testing.T) {
	all := map[string]string{
		"TOPOS_AUTHORIZER_URL": "https://platform.example/authorize", "TOPOS_AUTHORIZER_TOKEN": "a",
		"TOPOS_IDENTITY_URL": "https://login.example/", "TOPOS_IDENTITY_CLIENT_ID": " host ", "TOPOS_IDENTITY_SECRET_FILE": "/run/host/secret",
		"TOPOS_SESSION_KEYS_URL": "https://platform.example/sessions/", "TOPOS_SESSION_KEYS_TOKEN": "k",
		"TOPOS_CELLA_URL": "https://cella.example", "TOPOS_ORIGO_URL": "https://origo.example/",
	}
	c, err := Load(RoleServe, serve(all))
	if err != nil {
		t.Fatal(err)
	}
	if c.IdentityURL != "https://login.example" || c.IdentityClientID != "host" || c.IdentitySecretFile != "/run/host/secret" ||
		c.SessionKeysURL != "https://platform.example/sessions" || c.SessionKeysToken != "k" || c.OrigoURL != "https://origo.example" {
		t.Fatalf("config %+v", c)
	}
	for name, mut := range map[string]map[string]string{
		"identity without its client":    {"TOPOS_IDENTITY_CLIENT_ID": ""},
		"identity without its secret":    {"TOPOS_IDENTITY_SECRET_FILE": ""},
		"identity url not a url":         {"TOPOS_IDENTITY_URL": "login"},
		"no authorizer":                  {"TOPOS_AUTHORIZER_URL": "", "TOPOS_AUTHORIZER_TOKEN": ""},
		"a cella bearer beside tokens":   {"TOPOS_CELLA_TOKEN_FILE": "/run/cella/token"},
		"a git credential beside tokens": {"TOPOS_ORIGO_TOKEN_FILE": "/run/origo/token"},
		"keys without their token":       {"TOPOS_SESSION_KEYS_TOKEN": ""},
		"keys url not a url":             {"TOPOS_SESSION_KEYS_URL": "keys"},
		"a models key beside keys":       {"TOPOS_MODELS_KEY": "installation-key"},
		"origo url not a url":            {"TOPOS_ORIGO_URL": "origo"},
		"cella without identity or file": {"TOPOS_IDENTITY_URL": ""},
	} {
		vars := maps.Clone(all)
		maps.Copy(vars, mut)
		if _, err := Load(RoleServe, serve(vars)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	keysOnly := map[string]string{"TOPOS_AUTHORIZER_URL": "https://platform.example/authorize", "TOPOS_AUTHORIZER_TOKEN": "a", "TOPOS_SESSION_KEYS_URL": "https://platform.example/sessions", "TOPOS_SESSION_KEYS_TOKEN": "k"}
	if _, err := Load(RoleServe, serve(keysOnly)); err != nil {
		t.Fatalf("session keys alone: %v", err)
	}
	r, err := Load(RoleRunner, env(map[string]string{"TOPOS_INTERNAL_URL": "http://toposd:8081", "TOPOS_RUNNER_TOKEN": "t", "TOPOS_MODELS_URL": "https://lux.example", "TOPOS_CELLA_URL": "https://cella.example", "TOPOS_ORIGO_URL": "https://origo.example"}))
	if err != nil || r.CellaTokenFile != "" || r.OrigoURL != "https://origo.example" || r.IdentityURL != "" {
		t.Fatalf("a runner role with no Cella bearer: %+v %v", r, err)
	}
}

// TestTheAppHost: the app host's root and audience are read in serve and
// in the runner role alike; the root needs the git host an app's
// repository is on, and is an http URL (spec 043).
func TestTheAppHost(t *testing.T) {
	base := map[string]string{"TOPOS_CELLA_URL": "https://cella.example", "TOPOS_CELLA_TOKEN_FILE": "/run/cella/token", "TOPOS_ORIGO_URL": "https://origo.example"}
	vars := maps.Clone(base)
	maps.Copy(vars, map[string]string{"TOPOS_APPS_URL": " https://api.example/v1/apps/ ", "TOPOS_APPS_AUDIENCE": " apps-host "})
	c, err := Load(RoleServe, serve(vars))
	if err != nil || c.AppsURL != "https://api.example/v1/apps" || c.AppsAudience != "apps-host" {
		t.Fatalf("config %+v %v", c, err)
	}
	if c, err := Load(RoleServe, serve(base)); err != nil || c.AppsURL != "" || c.AppsAudience != "" {
		t.Fatalf("no app host: %+v %v", c, err)
	}
	r, err := Load(RoleRunner, env(map[string]string{"TOPOS_INTERNAL_URL": "http://toposd:8081", "TOPOS_RUNNER_TOKEN": "t", "TOPOS_MODELS_URL": "https://lux.example",
		"TOPOS_CELLA_URL": "https://cella.example", "TOPOS_ORIGO_URL": "https://origo.example", "TOPOS_APPS_URL": "https://api.example/v1/apps"}))
	if err != nil || r.AppsURL != "https://api.example/v1/apps" {
		t.Fatalf("a runner role's app host: %+v %v", r, err)
	}
	for name, mut := range map[string]map[string]string{
		"not a url":        {"TOPOS_APPS_URL": "apps"},
		"without git host": {"TOPOS_APPS_URL": "https://api.example/v1/apps", "TOPOS_ORIGO_URL": ""},
	} {
		v := maps.Clone(base)
		maps.Copy(v, mut)
		if _, err := Load(RoleServe, serve(v)); err == nil || !strings.Contains(err.Error(), "TOPOS_APPS_URL") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestTheOrigoTokenFile: an installation without an identity provider
// names the file of its git host's credential, read in serve and in the
// runner role alike; the file needs the git host it is sent to, and an
// installation with an identity provider refuses it, since its sandboxes
// push with their agents' tokens.
func TestTheOrigoTokenFile(t *testing.T) {
	selfHosted := map[string]string{
		"TOPOS_CELLA_URL": "https://cella.example", "TOPOS_CELLA_TOKEN_FILE": "/run/cella/token",
		"TOPOS_ORIGO_URL": "https://origo.example", "TOPOS_ORIGO_TOKEN_FILE": " /run/origo/token ",
	}
	c, err := Load(RoleServe, serve(selfHosted))
	if err != nil || c.OrigoTokenFile != "/run/origo/token" || c.OrigoURL != "https://origo.example" {
		t.Fatalf("serve: %+v %v", c, err)
	}
	r, err := Load(RoleRunner, env(map[string]string{
		"TOPOS_INTERNAL_URL": "http://toposd:8081", "TOPOS_RUNNER_TOKEN": "t", "TOPOS_MODELS_URL": "https://lux.example",
		"TOPOS_ORIGO_URL": "https://origo.example", "TOPOS_ORIGO_TOKEN_FILE": "/run/origo/token",
	}))
	if err != nil || r.OrigoTokenFile != "/run/origo/token" {
		t.Fatalf("runner: %+v %v", r, err)
	}
	noHost := maps.Clone(selfHosted)
	delete(noHost, "TOPOS_ORIGO_URL")
	if _, err := Load(RoleServe, serve(noHost)); err == nil || !strings.Contains(err.Error(), "TOPOS_ORIGO_TOKEN_FILE needs TOPOS_ORIGO_URL") {
		t.Fatalf("a git credential with no git host: %v", err)
	}
	withIdentity := maps.Clone(selfHosted)
	delete(withIdentity, "TOPOS_CELLA_TOKEN_FILE")
	maps.Copy(withIdentity, map[string]string{
		"TOPOS_AUTHORIZER_URL": "https://platform.example/authorize", "TOPOS_AUTHORIZER_TOKEN": "a",
		"TOPOS_IDENTITY_URL": "https://login.example", "TOPOS_IDENTITY_CLIENT_ID": "host", "TOPOS_IDENTITY_SECRET_FILE": "/run/host/secret",
	})
	if _, err := Load(RoleServe, serve(withIdentity)); err == nil || !strings.Contains(err.Error(), "TOPOS_ORIGO_TOKEN_FILE is set beside TOPOS_IDENTITY_URL") {
		t.Fatalf("a git credential beside an identity provider: %v", err)
	}
}

// TestTheCellaLabels: TOPOS_CELLA_LABELS is read in serve and in the
// runner role as key=value pairs, and a pair that is not one, a key given
// twice, or a key under topos.latere.ai/, which Topos sets itself, stops
// the start; unset, there are none.
func TestTheCellaLabels(t *testing.T) {
	c, err := Load(RoleServe, serve(map[string]string{"TOPOS_CELLA_LABELS": " tenant.example/id = t-1 ,, tenant.example/principal=p "}))
	if err != nil || !maps.Equal(c.CellaLabels, map[string]string{"tenant.example/id": "t-1", "tenant.example/principal": "p"}) {
		t.Fatalf("serve: %v %v", c.CellaLabels, err)
	}
	r, err := Load(RoleRunner, env(map[string]string{"TOPOS_INTERNAL_URL": "http://toposd:8081", "TOPOS_RUNNER_TOKEN": "t", "TOPOS_MODELS_URL": "https://lux.example", "TOPOS_CELLA_LABELS": "a=b"}))
	if err != nil || r.CellaLabels["a"] != "b" {
		t.Fatalf("runner: %v %v", r.CellaLabels, err)
	}
	if c, err := Load(RoleServe, serve(nil)); err != nil || c.CellaLabels != nil {
		t.Fatalf("unset: %v %v", c.CellaLabels, err)
	}
	for raw, want := range map[string]string{
		"tenant":                    "not key=value",
		"=v":                        "not key=value",
		"k=":                        "not key=value",
		"k=a b":                     "not key=value",
		"k=1,k=2":                   "twice",
		"topos.latere.ai/session=x": "the ones Topos sets itself",
	} {
		if _, err := Load(RoleServe, serve(map[string]string{"TOPOS_CELLA_LABELS": raw})); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want a problem naming %q", raw, err, want)
		}
	}
}

// TestBasePathMustMatchPublicURL is spec 030's one address: the base
// path is the path of TOPOS_PUBLIC_URL, and with no base path the public
// URL has none, so every URL toposd writes is one it answers on.
func TestBasePathMustMatchPublicURL(t *testing.T) {
	for name, tc := range map[string]struct {
		public, base, want string
	}{
		"a capability prefix":          {"https://api.example.com/v1/agents", "/v1/agents", ""},
		"a trailing slash on the URL":  {"https://api.example.com/v1/agents/", "/v1/agents", ""},
		"neither has a path":           {"https://topos.example", "", ""},
		"the URL's path is another":    {"https://api.example.com/v1/models", "/v1/agents", "they must be equal"},
		"a base path and no URL path":  {"https://api.example.com", "/v1/agents", "they must be equal"},
		"a URL path and no base path":  {"https://api.example.com/v1/agents", "", "needs TOPOS_BASE_PATH=/v1/agents"},
		"a base path with no slash":    {"https://api.example.com/v1/agents", "v1/agents", "starts with /"},
		"a base path with a slash end": {"https://api.example.com/v1/agents", "/v1/agents/", "no trailing /"},
		"the root as a base path":      {"https://api.example.com/", "/", "starts with /"},
		"a base path to clean":         {"https://api.example.com/v1/agents", "/v1//agents", "starts with /"},
	} {
		c, err := Load(RoleServe, serve(map[string]string{"TOPOS_PUBLIC_URL": tc.public, "TOPOS_BASE_PATH": tc.base}))
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.want == "" && c.BasePath != tc.base:
			t.Errorf("%s: BasePath = %q, want %q", name, c.BasePath, tc.base)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want a problem naming %q", name, err, tc.want)
		}
	}
	// The runner reads neither variable, so a base path there is no
	// problem of its own.
	if _, err := Load(RoleRunner, env(map[string]string{"TOPOS_INTERNAL_URL": "http://toposd:8081", "TOPOS_RUNNER_TOKEN": "t", "TOPOS_MODELS_URL": "https://lux.example/anthropic", "TOPOS_BASE_PATH": "x"})); err != nil {
		t.Errorf("runner: %v", err)
	}
}

// TestSearchVariables (spec 047): both roles read the search service's
// URL and its key; a URL that is not one stops the start, the key needs
// the URL, and a server that mints session keys refuses the key, since
// its sessions search with their own.
func TestSearchVariables(t *testing.T) {
	keys := map[string]string{"TOPOS_AUTHORIZER_URL": "https://platform.example/authorize", "TOPOS_AUTHORIZER_TOKEN": "a", "TOPOS_SESSION_KEYS_URL": "https://platform.example/sessions", "TOPOS_SESSION_KEYS_TOKEN": "k"}
	c, err := Load(RoleServe, serve(map[string]string{"TOPOS_SEARCH_URL": " https://search.example/v1/search ", "TOPOS_SEARCH_KEY": " sk "}))
	if err != nil || c.SearchURL != "https://search.example/v1/search" || c.SearchKey != "sk" {
		t.Fatalf("serve: %+v %v", c, err)
	}
	withKeys := maps.Clone(keys)
	withKeys["TOPOS_SEARCH_URL"] = "https://search.example/v1/search"
	if c, err := Load(RoleServe, serve(withKeys)); err != nil || c.SearchURL == "" || c.SearchKey != "" {
		t.Fatalf("a server with session keys and a search URL: %+v %v", c, err)
	}
	r, err := Load(RoleRunner, env(map[string]string{"TOPOS_INTERNAL_URL": "http://toposd:8081", "TOPOS_RUNNER_TOKEN": "t", "TOPOS_MODELS_URL": "https://lux.example", "TOPOS_SEARCH_URL": "http://search.example"}))
	if err != nil || r.SearchURL != "http://search.example" {
		t.Fatalf("a runner: %+v %v", r, err)
	}
	keyBeside := maps.Clone(withKeys)
	keyBeside["TOPOS_SEARCH_KEY"] = "sk"
	for name, vars := range map[string]map[string]string{
		"a URL that is not one":     {"TOPOS_SEARCH_URL": "search"},
		"a URL with a query":        {"TOPOS_SEARCH_URL": "https://search.example/?q=1"},
		"a key without its URL":     {"TOPOS_SEARCH_KEY": "sk"},
		"a key beside session keys": keyBeside,
	} {
		if _, err := Load(RoleServe, serve(vars)); err == nil || !strings.Contains(err.Error(), "TOPOS_SEARCH_") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
