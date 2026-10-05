// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command toposd is the Topos server: it keeps agent definitions and the
// sessions people have with them, and runs sessions on its runners. This
// file is the entry point and holds wiring only: the subcommands, the
// configuration, the listeners, and the run group. The behavior lives in
// the packages under internal/ and in the exported packages at the module
// root.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"latere.ai/x/cella/client"

	"latere.ai/x/pkg/authkit/jwt"
	"latere.ai/x/pkg/health"
	"latere.ai/x/pkg/otel"

	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/blob"
	"latere.ai/x/topos/internal/config"
	"latere.ai/x/topos/internal/credentials"
	"latere.ai/x/topos/internal/hosted"
	idp "latere.ai/x/topos/internal/identity"
	"latere.ai/x/topos/internal/publish"
	"latere.ai/x/topos/internal/runnerapi"
	"latere.ai/x/topos/internal/runnerrole"
	"latere.ai/x/topos/internal/server"
	"latere.ai/x/topos/internal/store"
	storedir "latere.ai/x/topos/internal/store/dir"
	"latere.ai/x/topos/internal/store/postgres"
	"latere.ai/x/topos/internal/token"
	"latere.ai/x/topos/internal/version"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/dialect"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	sessiondir "latere.ai/x/topos/session/dir"
)

// Shutdown timing of spec 002: readiness answers 503 at once, the drain
// delay lets a load balancer notice, then the servers close with the
// grace period.
var (
	drainDelay  = 3 * time.Second
	gracePeriod = 60 * time.Second
)

// pending names the spec that builds each role not built yet. A role
// listed here exits 1 with one line naming its spec, so a manifest that
// runs it fails loudly instead of idling.
var pending = map[string]string{
	"check": "028",
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run dispatches the subcommand and returns the process exit code, so
// tests drive it without a subprocess: 0 on a clean stop, 1 on a start-up
// or runtime failure, 2 on a usage error.
func run(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	name, rest := subcommand(args)
	switch name {
	case "", "serve":
		return serve(ctx, rest, getenv, stdout, stderr)
	case "token":
		return signToken(rest, getenv, stdout, stderr)
	case "runner":
		return runnerRole(ctx, rest, getenv, stdout, stderr)
	}
	if spec, ok := pending[name]; ok {
		_, _ = fmt.Fprintf(stderr, "toposd: %s is not built yet; spec %s builds it\n", name, spec)
		return 1
	}
	_, _ = fmt.Fprintf(stderr, "toposd: unknown subcommand %q; one of serve (the default), runner, check, token\n", name)
	return 2
}

// subcommand is spec 002's rule: the first argument that does not start
// with a dash names the subcommand, and the arguments around it are the
// subcommand's own.
func subcommand(args []string) (string, []string) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			rest := make([]string, 0, len(args)-1)
			rest = append(rest, args[:i]...)
			return a, append(rest, args[i+1:]...)
		}
	}
	return "", args
}

// serve is the server: the two listeners and the probes of spec 002, the
// API of spec 015 on the public listener, and its store. The in-process
// runners of spec 016 mount here.
func serve(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("toposd serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the build identity and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, version.String("toposd"))
		return 0
	}

	cfg, err := config.Load(config.RoleServe, getenv)
	if err != nil {
		return fail(stderr, err)
	}
	id, err := newIdentity(ctx, cfg)
	if err != nil {
		return fail(stderr, err)
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	st, err := openStores(ctx, cfg, log)
	if err != nil {
		return fail(stderr, err)
	}
	defer func() {
		if err := st.close(); err != nil {
			_, _ = fmt.Fprintf(stderr, "toposd: close the store: %v\n", err)
		}
	}()
	queue := runner.NewQueue(st.sessions, 0)
	// The doors are read once, for the runners and for the API's check of
	// a session's model at its create and at a switch, which routes a
	// model as its runner does whether or not this server runs sessions.
	doors, err := discoverDoors(ctx, cfg)
	if err != nil {
		return fail(stderr, err)
	}
	runnable, err := hosted.Runnable(hosted.Options{ModelsURL: cfg.ModelsURL, ModelsKey: cfg.ModelsKey, Doors: doors.Under(cfg.ModelsURL)})
	if err != nil {
		return fail(stderr, err)
	}
	so := server.Options{
		Sessions: st.sessions, Objects: st.objects, Verifier: id.verifier, Guard: id.guard,
		PublicURL: cfg.PublicURL, BasePath: cfg.BasePath, Log: log, Notify: queue.Notify, HostSessions: cfg.HostSessions,
		Cella: cfg.CellaURL != "", Runnable: runnable,
	}
	minter, identities, err := newMinter(cfg, st)
	if err != nil {
		return fail(stderr, err)
	}
	if identities != nil {
		so.Identities = identities
	}
	if cfg.HostSessions {
		so.Deleted = func(id string) error { return hosted.RemoveHostSession(cfg.DataDir, id) }
	}
	so.Workspaces = workspaces(cfg, minter)
	api, err := server.New(so)
	if err != nil {
		return fail(stderr, err)
	}
	// The runners stop with the process, and on every other way out of
	// serve too; each drive then leaves its session to the next runner.
	runCtx, stopRunners := context.WithCancel(ctx)
	var local func(string, session.Lease) runner.Credentials
	if minter != nil {
		local = minter.Local
	}
	runners, err := startRunners(runCtx, cfg, getenv, doors, st.sessions, queue, runner.KindServe, local, api.Failover, log)
	if err != nil {
		stopRunners()
		return fail(stderr, err)
	}
	defer func() {
		stopRunners()
		<-runners
	}()
	// The reaper of spec 014 ends expired sessions and deletes the ones
	// past their retention, and the minute loop of spec 022 fires the
	// schedules due and sends the held firings, until the process stops.
	go every(runCtx, server.ReapInterval, "reap sessions", api.Reap, log)
	go every(runCtx, server.TickInterval, "fire triggers", api.Tick, log)
	// The identities a crash left archived and undisabled are caught up
	// at start as well as on every reaper pass (spec 018).
	go func() {
		if err := api.Reconcile(runCtx); err != nil {
			log.ErrorContext(runCtx, "reconcile the agents' identities", "err", err)
		}
	}()

	draining := make(chan struct{})
	probes := health.Handler(health.Options{
		Ready:     health.Checks(health.Check{Name: "draining", Run: notDraining(draining)}, health.Check{Name: "store", Run: st.ping}),
		Timeout:   2 * time.Second,
		Version:   version.Version,
		Commit:    version.Commit,
		BuildTime: version.Date,
	})

	public := http.NewServeMux()
	for _, p := range []string{"/livez", "/readyz", "/version"} {
		public.Handle("GET "+p, probes)
	}
	public.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintln(w, version.String("toposd"))
	})
	// The API answers under its base path, and not_found for every path
	// the patterns above and below do not name.
	public.Handle("/", api.Handler())
	if id.signer != nil {
		jwks := id.signer.JWKS()
		// The key set sits under the public URL, whose path is the base
		// path, beside the API's routes (spec 030).
		public.HandleFunc("GET "+cfg.BasePath+token.JWKSPath, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "public, max-age=300")
			_, _ = w.Write(jwks)
		})
	}

	var lc net.ListenConfig
	publicLn, err := lc.Listen(ctx, "tcp", cfg.PublicAddr)
	if err != nil {
		return fail(stderr, fmt.Errorf("TOPOS_PUBLIC_ADDR: %w", err))
	}
	internalLn, err := lc.Listen(ctx, "tcp", cfg.InternalAddr)
	if err != nil {
		_ = publicLn.Close()
		return fail(stderr, fmt.Errorf("TOPOS_INTERNAL_ADDR: %w", err))
	}
	_, _ = fmt.Fprintf(stdout, "toposd: %s listening public=%s internal=%s store=%s\n",
		version.Version, publicLn.Addr(), internalLn.Addr(), st.name)

	internal := http.NewServeMux()
	internal.Handle("/", probes)
	if len(cfg.RunnerTokens) > 0 {
		ro := runnerapi.Options{Store: st.sessions, Queue: queue, Tokens: cfg.RunnerTokens, Failover: api.Failover, Log: log}
		if minter != nil {
			ro.Credentials = minter.Credential
		}
		routes, err := runnerapi.New(ro)
		if err != nil {
			return fail(stderr, err)
		}
		internal.Handle(runnerapi.Root+"/", routes.Handler())
		go routes.Reap(runCtx, reapInterval)
	}
	servers := []*http.Server{
		{Handler: public, ReadHeaderTimeout: 10 * time.Second},
		{Handler: internal, ReadHeaderTimeout: 10 * time.Second},
	}
	errc := make(chan error, len(servers))
	for i, ln := range []net.Listener{publicLn, internalLn} {
		go func(s *http.Server, ln net.Listener) {
			if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}(servers[i], ln)
	}

	select {
	case <-ctx.Done():
	case err := <-errc:
		return fail(stderr, err)
	}
	// The stop signal has fired, so the shutdown runs on a context that
	// keeps the request's values and outlives its cancellation.
	close(draining)
	stopping := context.WithoutCancel(ctx)
	sleepCtx(stopping, drainDelay)
	shutdownCtx, cancel := context.WithTimeout(stopping, gracePeriod)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdownCtx)
	}
	return 0
}

// every runs pass every interval until ctx ends, logging what it could
// not do under what.
func every(ctx context.Context, interval time.Duration, what string, pass func(context.Context) error, log *slog.Logger) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := pass(ctx); err != nil {
				log.ErrorContext(ctx, what, "err", err)
			}
		}
	}
}

// stores are the session store and the object store serve runs on, one
// Postgres or one data directory (spec 014).
type stores struct {
	sessions session.Store
	objects  store.Store
	name     string
	ping     func(context.Context) error
	close    func() error
}

// openStores opens Postgres when TOPOS_DB_URL names it, and the data
// directory otherwise, which one serving toposd holds alone.
func openStores(ctx context.Context, cfg config.Config, log *slog.Logger) (stores, error) {
	blobs, err := blob.Open(cfg.BlobURL, cfg.BlobAccessKey, cfg.BlobSecretKey, &http.Client{Transport: otel.Transport(nil)})
	if err != nil {
		return stores{}, fmt.Errorf("TOPOS_BLOB_URL: %w", err)
	}
	if cfg.DBURL != "" {
		pg, err := postgres.Open(ctx, cfg.DBURL, postgres.Options{PoolDSN: cfg.DBPoolURL, Log: log, Blobs: blobs})
		if err != nil {
			return stores{}, fmt.Errorf("TOPOS_DB_URL: %w", err)
		}
		return stores{sessions: pg, objects: pg, name: "postgres", ping: pg.Ping, close: func() error { pg.Close(); return nil }}, nil
	}
	release, err := storedir.LockServe(cfg.DataDir)
	if err != nil {
		return stores{}, fmt.Errorf("TOPOS_DATA_DIR %s: %w", cfg.DataDir, err)
	}
	sessions, err := sessiondir.OpenWith(cfg.DataDir, sessiondir.Options{Blobs: blobs})
	if err != nil {
		return stores{}, errors.Join(err, release())
	}
	objects, err := storedir.Open(cfg.DataDir, nil)
	if err != nil {
		return stores{}, errors.Join(err, release())
	}
	ping := func(context.Context) error {
		_, err := os.Stat(cfg.DataDir)
		return err
	}
	return stores{sessions: sessions, objects: objects, name: "dir:" + cfg.DataDir, ping: ping, close: release}, nil
}

// newMinter builds the minter of the sessions' own credentials and the
// identity provider it asks (spec 018); both are nil on a server that
// has neither an identity provider nor session keys.
func newMinter(cfg config.Config, st stores) (*credentials.Minter, *idp.Client, error) {
	if cfg.IdentityURL == "" && cfg.SessionKeysURL == "" {
		return nil, nil, nil
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: otel.Transport(nil)}
	o := credentials.Options{Sessions: st.sessions, Agents: st.objects}
	var ic *idp.Client
	if cfg.IdentityURL != "" {
		var err error
		if ic, err = idp.New(idp.Options{URL: cfg.IdentityURL, ClientID: cfg.IdentityClientID, SecretFile: cfg.IdentitySecretFile, HTTP: client}); err != nil {
			return nil, nil, fmt.Errorf("TOPOS_IDENTITY_URL: %w", err)
		}
		o.Tokens = ic
	}
	if cfg.SessionKeysURL != "" {
		o.Keys = &credentials.KeyRoutes{URL: cfg.SessionKeysURL, Token: cfg.SessionKeysToken, HTTP: client}
	}
	m, err := credentials.New(o)
	if err != nil {
		return nil, nil, err
	}
	return m, ic, nil
}

// workspaces reads the working directories of hosted sessions for the
// API's files route (spec 044): a sandbox with the session's own Cella
// token when the installation mints one, as a drive presents it, and
// with TOPOS_CELLA_TOKEN_FILE's bearer otherwise; a host session's
// directory when TOPOS_HOST_SESSIONS is on.
func workspaces(cfg config.Config, minter *credentials.Minter) func(context.Context, session.Session) (machine.FileReader, error) {
	o := hosted.WorkspaceOptions{CellaURL: cfg.CellaURL}
	if cfg.CellaTokenFile != "" {
		o.CellaToken = client.TokenFile(cfg.CellaTokenFile)
	}
	if minter != nil {
		// A read holds no lease; the tag names the API among the holders
		// of a session's credentials.
		o.Mint = func(ctx context.Context, id, audience, workload string) (runner.Credential, error) {
			return minter.Credential(ctx, id, "api", audience, workload)
		}
	}
	if cfg.HostSessions {
		o.DataDir = cfg.DataDir
	}
	return hosted.Workspaces(o)
}

// reapInterval is how often serve frees the claims of remote runners
// that stopped renewing them.
var reapInterval = 5 * time.Second

// startRunners starts the runners that drive hosted sessions,
// TOPOS_RUNNER_CAPACITY of them at once, claiming from queue and running
// over st, and returns a channel closed once they have stopped with ctx.
// A capacity of zero runs none. With TOPOS_HOST_SESSIONS=on the host
// sandbox is checked first, and a sandbox that does not run or does not
// confine stops the start. failover is the server's question of which
// model a turn continues on when its model cannot serve (spec 051), nil
// in a runner process, whose leases ask it over the internal listener.
func startRunners(ctx context.Context, cfg config.Config, getenv config.Getenv, doors models.Doors, st session.Store, queue runner.Claimer, kind string, creds func(string, session.Lease) runner.Credentials, failover func(context.Context, string, session.ModelRef, session.ModelRef, string) (session.ModelRef, error), log *slog.Logger) (<-chan struct{}, error) {
	done := make(chan struct{})
	if cfg.RunnerCapacity == 0 {
		close(done)
		return done, nil
	}
	// The runners reach each door under TOPOS_MODELS_URL itself, which
	// may be an address inside the installation's network, and a sandbox
	// reaches Lux at the root Lux published its doors under, since a
	// sandbox leaves only through Cella's egress gateway, toward public
	// hosts.
	cella := hosted.Cella(hosted.CellaOptions{})
	if cfg.CellaURL != "" {
		helpers, err := hosted.ReadHelpers(cfg.MachineHelpers)
		if err != nil {
			return nil, fmt.Errorf("TOPOS_MACHINE_HELPERS: %w", err)
		}
		co := hosted.CellaOptions{URL: cfg.CellaURL, Helpers: helpers, Dir: cfg.MachineDir, ModelsURL: cmp.Or(doors.Root(), cfg.ModelsURL), OrigoURL: cfg.OrigoURL, Labels: cfg.CellaLabels, Log: log}
		if cfg.CellaTokenFile != "" {
			co.Token = client.TokenFile(cfg.CellaTokenFile)
		}
		if cfg.OrigoTokenFile != "" {
			co.OrigoToken = client.TokenFile(cfg.OrigoTokenFile)
		}
		cella = hosted.Cella(co)
	}
	var onHost hosted.Machines
	if cfg.HostSessions {
		var err error
		if onHost, err = hostSessions(ctx, cfg, getenv); err != nil {
			return nil, fmt.Errorf("TOPOS_HOST_SESSIONS: %w", err)
		}
	}
	machines := hosted.ByKind(cella, onHost)
	// The app host a session publishes to (spec 043), reached by the
	// runner with the session's own token.
	apps := publish.Options{URL: cfg.AppsURL, Audience: cfg.AppsAudience, GitURL: cfg.OrigoURL}
	h, err := hosted.Harness(hosted.Options{Store: st, ModelsURL: cfg.ModelsURL, ModelsKey: cfg.ModelsKey, Doors: doors.Under(cfg.ModelsURL), Machines: machines, SearchURL: cfg.SearchURL, SearchKey: cfg.SearchKey, Publish: apps})
	if err != nil {
		return nil, err
	}
	host, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	// A session that works in a repository on the git host keeps its
	// checkpoints there, so a fork restores its files after the sandbox
	// is gone (spec 035).
	r, err := runner.New(runner.Options{Store: st, Harness: h, ID: fmt.Sprintf("%s-%s-%d", kind, host, os.Getpid()), Kind: kind, Credentials: creds, Failover: failover, CheckpointHost: cfg.OrigoURL})
	if err != nil {
		return nil, err
	}
	go func() {
		defer close(done)
		report := func(id string, err error) { log.ErrorContext(ctx, "runner", "session", id, "err", err) }
		if err := r.Serve(ctx, queue, cfg.RunnerCapacity, report); err != nil {
			report("", err)
		}
	}()
	return done, nil
}

// discoverDoors reads the family doors TOPOS_MODELS_URL names when it
// is a Lux root, whose discovery document names each family's door; a
// URL that does not answer stops the start, as an issuer that does not
// answer does.
func discoverDoors(ctx context.Context, cfg config.Config) (models.Doors, error) {
	doors, err := dialect.Discover(ctx, &http.Client{Timeout: 10 * time.Second, Transport: otel.Transport(nil)}, cfg.ModelsURL)
	if err != nil {
		return nil, fmt.Errorf("TOPOS_MODELS_URL: %w", err)
	}
	return doors, nil
}

// runnerRole is the runner role (spec 016): it claims hosted sessions
// from a toposd's internal listener and runs them, and serves only the
// probes. Every connection is its own; toposd never dials it.
func runnerRole(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("toposd runner", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(config.RoleRunner, getenv)
	if err != nil {
		return fail(stderr, err)
	}
	client, err := runnerrole.New(cfg.InternalURL, cfg.RunnerTokens[0], &http.Client{Transport: otel.Transport(nil)})
	if err != nil {
		return fail(stderr, err)
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	client.SetLog(log)
	draining := make(chan struct{})
	probes := health.Handler(health.Options{
		Ready:   health.Checks(health.Check{Name: "draining", Run: notDraining(draining)}),
		Timeout: 2 * time.Second, Version: version.Version, Commit: version.Commit, BuildTime: version.Date,
	})
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.InternalAddr)
	if err != nil {
		return fail(stderr, fmt.Errorf("TOPOS_INTERNAL_ADDR: %w", err))
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	// A claim's lease reaches its session's credentials over the token
	// route itself, so the runner role builds none.
	doors, err := discoverDoors(ctx, cfg)
	if err != nil {
		return fail(stderr, errors.Join(err, ln.Close()))
	}
	runners, err := startRunners(runCtx, cfg, getenv, doors, client, client, runner.KindRunner, nil, nil, log)
	if err != nil {
		return fail(stderr, errors.Join(err, ln.Close()))
	}
	srv := &http.Server{Handler: probes, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	_, _ = fmt.Fprintf(stdout, "toposd: %s runner claiming from %s internal=%s\n", version.Version, cfg.InternalURL, ln.Addr())
	code := 0
	select {
	case <-ctx.Done():
	case err := <-errc:
		code = fail(stderr, err)
	}
	close(draining)
	stop()
	<-runners
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gracePeriod)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && code == 0 {
		code = fail(stderr, err)
	}
	return code
}

// identity is what the API asks through (spec 006): the verifier, the
// guard over the installation's authorizer or the owner policy, and the
// local issuer when TOPOS_LOCAL_ISSUER_KEY is set.
type identity struct {
	verifier *auth.Verifier
	guard    auth.Guard
	signer   *token.Signer
}

// newIdentity builds the identity of a serving toposd. Every listed
// issuer's key set is read here, so an issuer that does not answer stops
// the start.
func newIdentity(ctx context.Context, cfg config.Config) (identity, error) {
	var id identity
	client := &http.Client{Timeout: 10 * time.Second, Transport: otel.Transport(nil)}
	o := auth.Options{Issuers: cfg.OIDCIssuers, Audiences: cfg.OIDCAudiences, HTTP: client}
	if cfg.LocalIssuerKey != "" {
		s, err := token.New(cfg.PublicURL, cfg.LocalIssuerKey, nil)
		if err != nil {
			return identity{}, err
		}
		id.signer = s
		o.LocalIssuer, o.LocalKeys = s.Issuer(), []jwt.LocalKey{s.LocalKey()}
	}
	v, err := auth.NewVerifier(ctx, o)
	if err != nil {
		return identity{}, err
	}
	a, err := auth.NewAuthorizer(auth.AuthorizerOptions{URL: cfg.AuthorizerURL, Token: cfg.AuthorizerToken, Admins: cfg.AdminSubjects, HTTP: client})
	if err != nil {
		return identity{}, fmt.Errorf("TOPOS_AUTHORIZER_URL: %w", err)
	}
	id.verifier, id.guard = v, auth.Guard{Authorizer: a}
	return id, nil
}

// signToken is the token role: it signs one token with the local
// issuer's key and prints it, and opens no store.
func signToken(args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("toposd token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	subject := fs.String("subject", "admin", "the sub claim")
	ttl := fs.Duration("ttl", time.Hour, "the lifetime, at most 24h")
	audience := fs.String("audience", "", "the aud claim; the first of TOPOS_OIDC_AUDIENCE when empty")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "toposd: token takes no argument, got %q\n", fs.Args())
		return 2
	}
	cfg, err := config.Load(config.RoleToken, getenv)
	if err != nil {
		return fail(stderr, err)
	}
	s, err := token.New(cfg.PublicURL, cfg.LocalIssuerKey, nil)
	if err != nil {
		return fail(stderr, err)
	}
	aud := *audience
	if aud == "" {
		aud = cfg.OIDCAudiences[0]
	}
	raw, err := s.Sign(*subject, aud, *ttl)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "toposd: token: %v\n", err)
		return 2
	}
	_, _ = fmt.Fprintln(stdout, raw)
	return 0
}

// fail writes the one line an operator reads on a start-up or runtime
// failure and returns exit code 1.
func fail(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "toposd: %v\n", err)
	return 1
}

// notDraining fails readiness once shutdown has begun, so a load balancer
// stops routing before the servers close.
func notDraining(draining <-chan struct{}) func(context.Context) error {
	return func(context.Context) error {
		select {
		case <-draining:
			return errors.New("shutting down")
		default:
			return nil
		}
	}
}

// sleepCtx waits d or until ctx ends, whichever is first.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
