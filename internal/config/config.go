// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package config reads the typed configuration of toposd from the
// environment. Every variable spec 002 names is read once at start-up, and
// a start-up with anything missing or malformed fails with one message that
// lists every problem, so an operator fixes a deployment in one round.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"sort"
	"strings"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/internal/token"
)

// Defaults for the optional variables.
const (
	DefaultPublicAddr   = ":8080"
	DefaultInternalAddr = ":8081"
	DefaultAudience     = "topos"
)

// The roles of spec 002. A role reads the variables its row names, and
// a variable another role requires is not a problem for it.
const (
	RoleServe  = "serve"
	RoleRunner = "runner"
	RoleCheck  = "check"
	RoleToken  = "token"
)

// Getenv is the environment lookup Load reads through, so a test passes a
// map and never touches the process environment.
type Getenv func(string) string

// Config is the resolved configuration. Field names follow the variable
// names in spec 002 without the TOPOS_ prefix. The variables later specs
// own join here with those specs.
type Config struct {
	// PublicAddr is where the /v1 API and the public probes listen.
	PublicAddr string
	// InternalAddr is where the probes and metrics listen for the cluster,
	// and, from spec 016, the runner protocol.
	InternalAddr string
	// DBURL is the Postgres the migrations and LISTEN use, directly;
	// empty keeps sessions in the directory store (spec 014).
	DBURL string
	// DBPoolURL serves queries through a transaction-pooling proxy; empty
	// serves them on DBURL.
	DBPoolURL string
	// PublicURL is the absolute URL clients reach the public listener at,
	// without a trailing slash, and the local issuer's name.
	PublicURL string
	// OIDCIssuers are the issuers whose tokens the API accepts, and
	// OIDCAudiences the audiences a token may carry, the first primary.
	OIDCIssuers   []string
	OIDCAudiences []string
	// OIDCInsecureIssuers may use http:// on a host other than loopback.
	OIDCInsecureIssuers []string
	// AuthorizerURL and AuthorizerToken are the installation's
	// authorizer; an empty URL selects the owner policy.
	AuthorizerURL   string
	AuthorizerToken string
	// AdminSubjects act on every object under the owner policy.
	AdminSubjects []string
	// LocalIssuerKey is the PEM private key of the local issuer; empty
	// is no local issuer.
	LocalIssuerKey string
}

// Load reads the variables role reads through getenv and returns the
// configuration, or one error naming every problem found, sorted by
// variable name.
func Load(role string, getenv Getenv) (Config, error) {
	c := Config{
		PublicURL:      strings.TrimRight(strings.TrimSpace(getenv("TOPOS_PUBLIC_URL")), "/"),
		OIDCAudiences:  list(strings.TrimSpace(getenv("TOPOS_OIDC_AUDIENCE")), false),
		LocalIssuerKey: strings.TrimSpace(getenv("TOPOS_LOCAL_ISSUER_KEY")),
	}
	if len(c.OIDCAudiences) == 0 {
		c.OIDCAudiences = []string{DefaultAudience}
	}
	var problems []string
	if role == RoleToken {
		if c.PublicURL == "" {
			problems = append(problems, "TOPOS_PUBLIC_URL is required; it is the iss of the token")
		}
		if c.LocalIssuerKey == "" {
			problems = append(problems, "TOPOS_LOCAL_ISSUER_KEY is required; it signs the token")
		}
		problems = append(problems, c.checkIdentity()...)
		return done(c, problems)
	}
	c.PublicAddr = withDefault(getenv("TOPOS_PUBLIC_ADDR"), DefaultPublicAddr)
	c.InternalAddr = withDefault(getenv("TOPOS_INTERNAL_ADDR"), DefaultInternalAddr)
	c.DBURL = strings.TrimSpace(getenv("TOPOS_DB_URL"))
	c.DBPoolURL = strings.TrimSpace(getenv("TOPOS_DB_POOL_URL"))
	c.OIDCIssuers = list(strings.TrimSpace(getenv("TOPOS_OIDC_ISSUERS")), true)
	c.OIDCInsecureIssuers = list(strings.TrimSpace(getenv("TOPOS_OIDC_INSECURE_ISSUERS")), true)
	c.AuthorizerURL = strings.TrimSpace(getenv("TOPOS_AUTHORIZER_URL"))
	c.AuthorizerToken = strings.TrimSpace(getenv("TOPOS_AUTHORIZER_TOKEN"))
	c.AdminSubjects = authz.ParseSubjects(strings.TrimSpace(getenv("TOPOS_ADMIN_SUBJECTS")))
	if err := checkAddr(c.PublicAddr); err != nil {
		problems = append(problems, "TOPOS_PUBLIC_ADDR "+err.Error())
	}
	if err := checkAddr(c.InternalAddr); err != nil {
		problems = append(problems, "TOPOS_INTERNAL_ADDR "+err.Error())
	}
	if c.DBPoolURL != "" && c.DBURL == "" {
		problems = append(problems, "TOPOS_DB_POOL_URL needs TOPOS_DB_URL, which the migrations and LISTEN use")
	}
	for name, v := range map[string]string{"TOPOS_DB_URL": c.DBURL, "TOPOS_DB_POOL_URL": c.DBPoolURL} {
		if v != "" && !strings.HasPrefix(v, "postgres://") && !strings.HasPrefix(v, "postgresql://") {
			problems = append(problems, name+" must be a postgres:// URL")
		}
	}
	if sameEndpoint(c.PublicAddr, c.InternalAddr) {
		problems = append(problems, "TOPOS_INTERNAL_ADDR must differ from TOPOS_PUBLIC_ADDR; both are "+c.PublicAddr)
	}
	if role == RoleServe || role == RoleCheck {
		if c.PublicURL == "" {
			problems = append(problems, "TOPOS_PUBLIC_URL is required; it is the base of every URL toposd writes")
		}
		if len(c.OIDCIssuers) == 0 && c.LocalIssuerKey == "" {
			problems = append(problems, "TOPOS_OIDC_ISSUERS is required unless TOPOS_LOCAL_ISSUER_KEY is set; there is no anonymous access")
		}
	}
	problems = append(problems, c.checkIdentity()...)
	return done(c, problems)
}

func done(c Config, problems []string) (Config, error) {
	if len(problems) > 0 {
		sort.Strings(problems)
		return Config{}, errors.New("configuration: " + strings.Join(problems, "; "))
	}
	return c, nil
}

// checkIdentity is the shape of the identity variables that are set.
func (c Config) checkIdentity() []string {
	var problems []string
	if c.PublicURL != "" {
		if err := checkURL(c.PublicURL); err != nil {
			problems = append(problems, "TOPOS_PUBLIC_URL "+err.Error())
		}
	}
	for _, iss := range c.OIDCIssuers {
		if err := checkURL(iss); err != nil {
			problems = append(problems, "TOPOS_OIDC_ISSUERS "+err.Error())
			continue
		}
		u, _ := url.Parse(iss)
		if u.Scheme == "http" && !loopback(u.Hostname()) && !slices.Contains(c.OIDCInsecureIssuers, iss) {
			problems = append(problems, "TOPOS_OIDC_ISSUERS lists "+iss+", which is not https; only loopback and TOPOS_OIDC_INSECURE_ISSUERS may use http")
		}
	}
	for _, iss := range c.OIDCInsecureIssuers {
		if !slices.Contains(c.OIDCIssuers, iss) {
			problems = append(problems, "TOPOS_OIDC_INSECURE_ISSUERS names "+iss+", which TOPOS_OIDC_ISSUERS does not list")
		}
	}
	if c.AuthorizerURL != "" {
		if err := checkURL(c.AuthorizerURL); err != nil {
			problems = append(problems, "TOPOS_AUTHORIZER_URL "+err.Error())
		}
		if c.AuthorizerToken == "" {
			problems = append(problems, "TOPOS_AUTHORIZER_URL needs TOPOS_AUTHORIZER_TOKEN, the bearer the authorizer requires")
		}
	}
	if c.LocalIssuerKey != "" && c.PublicURL != "" {
		if _, err := token.New(c.PublicURL, c.LocalIssuerKey, nil); err != nil {
			problems = append(problems, err.Error())
		}
	}
	return problems
}

// list reads a comma-separated variable, trimming each entry, and a
// trailing slash from an issuer, and dropping empties.
func list(raw string, issuers bool) []string {
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		p := strings.TrimSpace(part)
		if issuers {
			p = strings.TrimRight(p, "/")
		}
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// checkURL accepts an absolute http or https URL with a host and no
// query or fragment.
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("is %q, not an absolute http or https URL", raw)
	}
	return nil
}

// loopback reports whether host names this machine.
func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func withDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// sameEndpoint reports whether two valid addresses name one socket. Port
// 0 asks the kernel for any free port, so two ":0" addresses are two
// sockets and a test that binds both on loopback is not refused.
func sameEndpoint(a, b string) bool {
	if a != b {
		return false
	}
	_, port, err := net.SplitHostPort(a)
	return err == nil && port != "0"
}

// checkAddr accepts what net.Listen accepts for a TCP address: host:port
// with the host optional.
func checkAddr(addr string) error {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("is %q, not a host:port address", addr)
	}
	return nil
}
