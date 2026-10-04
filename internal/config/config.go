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
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
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
	// BasePath is TOPOS_BASE_PATH, the path the API answers under in the
	// place of /v1 (spec 030), and then the path of PublicURL; empty
	// serves the API under /v1 and PublicURL has no path.
	BasePath string
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
	// BlobURL is where the store keeps blob bodies: file:///<path> or
	// s3://<host>/<bucket>/<prefix>; empty keeps them in the store
	// (spec 014). BlobAccessKey and BlobSecretKey are the object store's
	// credential.
	BlobURL       string
	BlobAccessKey string
	BlobSecretKey string
	// DataDir is TOPOS_DATA_DIR resolved: the directory store's root, and
	// the checkpoint and worktree directories beneath it.
	DataDir string
	// ModelsURL and ModelsKey are the installation's model connection:
	// the base URL of an agent that names none, a Lux door or a
	// provider's API, and the credential of an agent that names none.
	ModelsURL string
	ModelsKey string
	// RunnerCapacity is how many sessions serve drives at once; zero runs
	// no runner in process.
	RunnerCapacity int
	// CellaURL is the Cella control plane hosted sessions' machines are
	// created on; empty refuses them machine_unavailable. CellaTokenFile
	// is the file holding the bearer toposd presents to Cella, read on
	// every request so it can be rotated in place. MachineHelpers is the
	// directory of the topos-machine builds the machines upload.
	CellaURL       string
	CellaTokenFile string
	MachineHelpers string
	// CellaLabels are TOPOS_CELLA_LABELS: labels every sandbox and every
	// sandbox Secret toposd creates in Cella carries beside the session's
	// and the agent's, such as the tenant a Cella's authorizer requires of
	// what a caller creates.
	CellaLabels map[string]string
	// MachineDir is where the helper lives inside each sandbox; empty is
	// the machine's default under /tmp.
	MachineDir string
	// RunnerTokens are TOPOS_RUNNER_TOKEN: the bearers of the runner
	// routes, the first the one the runner role sends. Empty mounts no
	// runner route on serve.
	RunnerTokens []string
	// InternalURL is where the runner role reaches a toposd's internal
	// listener.
	InternalURL string
	// HostSessions is TOPOS_HOST_SESSIONS=on: the role runs hosted
	// sessions whose agent asks for a host machine on its own host, each
	// inside the mandatory host sandbox and a directory of its own under
	// DataDir. Off, such a session is refused machine_unavailable.
	HostSessions bool
	// OrigoURL is the git host a hosted session's sandbox pushes to with
	// its agent's token (spec 018), and where a session that works in a
	// repository there keeps its checkpoints (spec 035).
	OrigoURL string
	// OrigoTokenFile is the file holding the git host's credential of an
	// installation without an identity provider, which each sandbox's git
	// sends to OrigoURL in the place of an agent's token; read at each
	// sandbox's open, so it can be rotated in place.
	OrigoTokenFile string
	// IdentityURL is the identity provider that hosts the installation's
	// agents; empty gives agents no identity (spec 018). IdentityClientID
	// is the host client toposd authenticates as, and IdentitySecretFile
	// the file holding its secret, read at each fetch of its token.
	IdentityURL        string
	IdentityClientID   string
	IdentitySecretFile string
	// SessionKeysURL and SessionKeysToken are the authorizer's session key
	// routes and their bearer, where toposd registers each session's Lux
	// keys by hash; an empty URL mints no session key (spec 018).
	SessionKeysURL   string
	SessionKeysToken string
	// SearchURL is the search service web_search sends its queries to,
	// empty for none (spec 040); SearchKey is the bearer sent to it by an
	// installation that mints no session keys, whose sessions search with
	// their own.
	SearchURL string
	SearchKey string
}

// Defaults of the runner variables.
const (
	DefaultRunnerCapacity = 16
	DefaultMachineHelpers = "/usr/local/lib/topos"
)

// DataDir is TOPOS_DATA_DIR, or $XDG_STATE_HOME/topos, or
// .local/state/topos under the Home directory.
func DataDir(getenv Getenv) (string, error) {
	if d := strings.TrimSpace(getenv("TOPOS_DATA_DIR")); d != "" {
		return d, nil
	}
	if d := strings.TrimSpace(getenv("XDG_STATE_HOME")); d != "" {
		return filepath.Join(d, "topos"), nil
	}
	if h := Home(getenv); h != "" {
		return filepath.Join(h, ".local", "state", "topos"), nil
	}
	return "", errors.New("no data directory: set TOPOS_DATA_DIR or HOME")
}

// Home is the person's home directory: HOME, or USERPROFILE, which
// Windows sets where HOME is unset outside a POSIX shell; empty when
// neither is set.
func Home(getenv Getenv) string {
	if h := strings.TrimSpace(getenv("HOME")); h != "" {
		return h
	}
	return strings.TrimSpace(getenv("USERPROFILE"))
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
	c.RunnerTokens = list(strings.TrimSpace(getenv("TOPOS_RUNNER_TOKEN")), false)
	if role == RoleRunner {
		problems = append(problems, c.readRunner(getenv)...)
		c.InternalURL = strings.TrimRight(strings.TrimSpace(getenv("TOPOS_INTERNAL_URL")), "/")
		if c.InternalURL == "" {
			problems = append(problems, "TOPOS_INTERNAL_URL is required; it is the toposd internal listener the runner claims from")
		} else if err := checkURL(c.InternalURL); err != nil {
			problems = append(problems, "TOPOS_INTERNAL_URL "+err.Error())
		}
		if len(c.RunnerTokens) == 0 {
			problems = append(problems, "TOPOS_RUNNER_TOKEN is required; the runner sends its first bearer")
		}
		if c.RunnerCapacity == 0 {
			problems = append(problems, "TOPOS_RUNNER_CAPACITY is 0, and a runner that runs no session has nothing to do")
		}
	}
	// A runner role keeps nothing of its own, so it reads the data
	// directory only for the session directories of host sessions.
	if role == RoleServe || role == RoleCheck || (role == RoleRunner && c.HostSessions) {
		var err error
		if c.DataDir, err = DataDir(getenv); err != nil {
			problems = append(problems, "TOPOS_DATA_DIR is unset, and so are XDG_STATE_HOME and HOME")
		}
	}
	if role == RoleServe || role == RoleCheck {
		problems = append(problems, c.readRunner(getenv)...)
		problems = append(problems, c.readCredentials(getenv)...)
		if c.PublicURL == "" {
			problems = append(problems, "TOPOS_PUBLIC_URL is required; it is the base of every URL toposd writes")
		}
		if len(c.OIDCIssuers) == 0 && c.LocalIssuerKey == "" {
			problems = append(problems, "TOPOS_OIDC_ISSUERS is required unless TOPOS_LOCAL_ISSUER_KEY is set; there is no anonymous access")
		}
		problems = append(problems, c.readBlobs(getenv)...)
		problems = append(problems, c.readBasePath(getenv)...)
	}
	problems = append(problems, c.checkIdentity()...)
	return done(c, problems)
}

// readBasePath reads TOPOS_BASE_PATH (spec 030). The base path and the
// path of TOPOS_PUBLIC_URL name one address: set, the two are equal, and
// unset, the public URL has no path, since the API then answers under
// /v1 at the listener's root. Either mismatch would have toposd write
// URLs it does not answer on.
func (c *Config) readBasePath(getenv Getenv) []string {
	c.BasePath = strings.TrimSpace(getenv("TOPOS_BASE_PATH"))
	if c.BasePath != "" && (!strings.HasPrefix(c.BasePath, "/") || c.BasePath == "/" || path.Clean(c.BasePath) != c.BasePath) {
		return []string{"TOPOS_BASE_PATH is " + strconv.Quote(c.BasePath) + ", not a path that starts with / and has no trailing /"}
	}
	u, err := url.Parse(c.PublicURL)
	if c.PublicURL == "" || err != nil {
		// The public URL's own problem is reported where it is read.
		return nil
	}
	have := strings.TrimRight(u.Path, "/")
	switch {
	case c.BasePath == "" && have != "":
		return []string{"TOPOS_PUBLIC_URL has the path " + have + ", which needs TOPOS_BASE_PATH=" + have}
	case c.BasePath != "" && have != c.BasePath:
		return []string{"TOPOS_BASE_PATH is " + c.BasePath + " and the path of TOPOS_PUBLIC_URL is " + strconv.Quote(have) + "; they must be equal"}
	}
	return nil
}

// readBlobs reads where the store keeps blob bodies: a file:// URL with
// an absolute path, or an s3:// URL with a host and a bucket and both
// keys of its credential.
func (c *Config) readBlobs(getenv Getenv) []string {
	c.BlobURL = strings.TrimSpace(getenv("TOPOS_BLOB_URL"))
	c.BlobAccessKey = strings.TrimSpace(getenv("TOPOS_BLOB_ACCESS_KEY"))
	c.BlobSecretKey = strings.TrimSpace(getenv("TOPOS_BLOB_SECRET_KEY"))
	if c.BlobURL == "" {
		return nil
	}
	u, err := url.Parse(c.BlobURL)
	switch {
	case err != nil:
		return []string{"TOPOS_BLOB_URL is not a URL"}
	case u.Scheme == "file" && u.Host == "" && strings.HasPrefix(u.Path, "/"):
		return nil
	case u.Scheme == "s3" && u.Host != "" && strings.Trim(u.Path, "/") != "":
		if c.BlobAccessKey == "" || c.BlobSecretKey == "" {
			return []string{"TOPOS_BLOB_URL names an s3:// store, which needs TOPOS_BLOB_ACCESS_KEY and TOPOS_BLOB_SECRET_KEY; there is no credential chain"}
		}
		return nil
	}
	return []string{"TOPOS_BLOB_URL is " + strconv.Quote(c.BlobURL) + ", neither file:///<absolute path> nor s3://<host>/<bucket>/<prefix>"}
}

// readRunner reads the variables of the in-process runners and the
// machines they open.
func (c *Config) readRunner(getenv Getenv) []string {
	var problems []string
	c.ModelsURL = strings.TrimRight(strings.TrimSpace(getenv("TOPOS_MODELS_URL")), "/")
	c.ModelsKey = strings.TrimSpace(getenv("TOPOS_MODELS_KEY"))
	switch {
	case c.ModelsURL == "":
		problems = append(problems, "TOPOS_MODELS_URL is required; it is the model connection of an agent that names none")
	case strings.HasPrefix(c.ModelsURL, "scripted:"):
		problems = append(problems, "TOPOS_MODELS_URL names a scripted model, which is for tests and never served")
	default:
		if err := checkURL(c.ModelsURL); err != nil {
			problems = append(problems, "TOPOS_MODELS_URL "+err.Error())
		}
	}
	c.RunnerCapacity = DefaultRunnerCapacity
	if v := strings.TrimSpace(getenv("TOPOS_RUNNER_CAPACITY")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			problems = append(problems, "TOPOS_RUNNER_CAPACITY is "+strconv.Quote(v)+", not a count of sessions")
		}
		c.RunnerCapacity = n
	}
	c.CellaURL = strings.TrimRight(strings.TrimSpace(getenv("TOPOS_CELLA_URL")), "/")
	c.CellaTokenFile = strings.TrimSpace(getenv("TOPOS_CELLA_TOKEN_FILE"))
	var bad []string
	c.CellaLabels, bad = cellaLabels(getenv("TOPOS_CELLA_LABELS"))
	problems = append(problems, bad...)
	c.MachineHelpers = withDefault(getenv("TOPOS_MACHINE_HELPERS"), DefaultMachineHelpers)
	c.MachineDir = strings.TrimSpace(getenv("TOPOS_MACHINE_DIR"))
	switch v := strings.TrimSpace(getenv("TOPOS_HOST_SESSIONS")); v {
	case "", "off":
	case "on":
		c.HostSessions = true
	default:
		problems = append(problems, "TOPOS_HOST_SESSIONS is "+strconv.Quote(v)+", either on or off")
	}
	if c.MachineDir != "" && !strings.HasPrefix(c.MachineDir, "/") {
		problems = append(problems, "TOPOS_MACHINE_DIR is "+strconv.Quote(c.MachineDir)+", not an absolute path inside the sandbox")
	}
	if c.CellaURL != "" {
		if err := checkURL(c.CellaURL); err != nil {
			problems = append(problems, "TOPOS_CELLA_URL "+err.Error())
		}
	}
	c.OrigoURL = strings.TrimRight(strings.TrimSpace(getenv("TOPOS_ORIGO_URL")), "/")
	if c.OrigoURL != "" {
		if err := checkURL(c.OrigoURL); err != nil {
			problems = append(problems, "TOPOS_ORIGO_URL "+err.Error())
		}
	}
	c.OrigoTokenFile = strings.TrimSpace(getenv("TOPOS_ORIGO_TOKEN_FILE"))
	if c.OrigoTokenFile != "" && c.OrigoURL == "" {
		problems = append(problems, "TOPOS_ORIGO_TOKEN_FILE needs TOPOS_ORIGO_URL, the git host its credential is sent to")
	}
	c.SearchURL = strings.TrimSpace(getenv("TOPOS_SEARCH_URL"))
	c.SearchKey = strings.TrimSpace(getenv("TOPOS_SEARCH_KEY"))
	if c.SearchURL != "" {
		if err := checkURL(c.SearchURL); err != nil {
			problems = append(problems, "TOPOS_SEARCH_URL "+err.Error())
		}
	} else if c.SearchKey != "" {
		problems = append(problems, "TOPOS_SEARCH_KEY needs TOPOS_SEARCH_URL, the search service it is sent to")
	}
	return problems
}

// readCredentials reads the variables of a server whose sessions act
// with credentials of their own (spec 018): the identity provider that
// hosts its agents and the authorizer's session key routes. Each needs
// the installation's authorizer, which alone records the sessions their
// credentials name, and neither sits beside the installation credential
// it replaces. A server without an identity provider presents
// TOPOS_CELLA_TOKEN_FILE to Cella, so its Cella URL needs one, and its
// sandboxes' git presents TOPOS_ORIGO_TOKEN_FILE when it is set.
func (c *Config) readCredentials(getenv Getenv) []string {
	var problems []string
	c.IdentityURL = strings.TrimRight(strings.TrimSpace(getenv("TOPOS_IDENTITY_URL")), "/")
	c.IdentityClientID = strings.TrimSpace(getenv("TOPOS_IDENTITY_CLIENT_ID"))
	c.IdentitySecretFile = strings.TrimSpace(getenv("TOPOS_IDENTITY_SECRET_FILE"))
	c.SessionKeysURL = strings.TrimRight(strings.TrimSpace(getenv("TOPOS_SESSION_KEYS_URL")), "/")
	c.SessionKeysToken = strings.TrimSpace(getenv("TOPOS_SESSION_KEYS_TOKEN"))
	authorizer := strings.TrimSpace(getenv("TOPOS_AUTHORIZER_URL")) != ""
	if c.IdentityURL != "" {
		if err := checkURL(c.IdentityURL); err != nil {
			problems = append(problems, "TOPOS_IDENTITY_URL "+err.Error())
		}
		if c.IdentityClientID == "" || c.IdentitySecretFile == "" {
			problems = append(problems, "TOPOS_IDENTITY_URL needs TOPOS_IDENTITY_CLIENT_ID and TOPOS_IDENTITY_SECRET_FILE, the host client toposd authenticates as")
		}
		if !authorizer {
			problems = append(problems, "TOPOS_IDENTITY_URL needs TOPOS_AUTHORIZER_URL: only the installation's authorizer checks the sessions its agents' tokens name")
		}
		if c.CellaTokenFile != "" {
			problems = append(problems, "TOPOS_CELLA_TOKEN_FILE is set beside TOPOS_IDENTITY_URL; sessions reach Cella with their agents' tokens, so unset it")
		}
		if c.OrigoTokenFile != "" {
			problems = append(problems, "TOPOS_ORIGO_TOKEN_FILE is set beside TOPOS_IDENTITY_URL; sandboxes reach the git host with their agents' tokens, so unset it")
		}
	} else if c.CellaURL != "" && c.CellaTokenFile == "" {
		problems = append(problems, "TOPOS_CELLA_URL needs TOPOS_CELLA_TOKEN_FILE, the bearer toposd presents to Cella, unless TOPOS_IDENTITY_URL mints its sessions' tokens")
	}
	if c.SessionKeysURL != "" {
		if err := checkURL(c.SessionKeysURL); err != nil {
			problems = append(problems, "TOPOS_SESSION_KEYS_URL "+err.Error())
		}
		if c.SessionKeysToken == "" {
			problems = append(problems, "TOPOS_SESSION_KEYS_URL needs TOPOS_SESSION_KEYS_TOKEN, the bearer its routes require")
		}
		if !authorizer {
			problems = append(problems, "TOPOS_SESSION_KEYS_URL needs TOPOS_AUTHORIZER_URL: the session keys are the authorizer's, for the sessions it recorded")
		}
		if c.ModelsKey != "" {
			problems = append(problems, "TOPOS_MODELS_KEY is set beside TOPOS_SESSION_KEYS_URL; sessions ask models with their own keys, so unset it")
		}
		if c.SearchKey != "" {
			problems = append(problems, "TOPOS_SEARCH_KEY is set beside TOPOS_SESSION_KEYS_URL; sessions search with their own keys, so unset it")
		}
	}
	return problems
}

// toposLabels is the prefix of the labels Topos sets itself on what it
// creates in Cella, which the installation's labels cannot name.
const toposLabels = "topos.latere.ai/"

// cellaLabels reads TOPOS_CELLA_LABELS, key=value pairs separated by
// commas, each key once, neither side empty or holding white space, and
// no key under topos.latere.ai/; nil when the variable is empty.
func cellaLabels(raw string) (map[string]string, []string) {
	var out map[string]string
	var problems []string
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch {
		case !ok || k == "" || v == "" || strings.ContainsAny(k+v, " \t\n\r"):
			problems = append(problems, "TOPOS_CELLA_LABELS has "+strconv.Quote(part)+", not key=value")
			continue
		case strings.HasPrefix(k, toposLabels):
			problems = append(problems, "TOPOS_CELLA_LABELS names "+k+"; the labels under "+toposLabels+" are the ones Topos sets itself")
			continue
		case out[k] != "":
			problems = append(problems, "TOPOS_CELLA_LABELS names "+k+" twice")
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out, problems
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
