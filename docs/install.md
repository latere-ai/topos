# Installing Topos

This page is for the person who runs a Topos server: what to pull, what
the server stores and where, which keys it holds, how to probe it, how to
upgrade it, and what a production installation needs around it. To try
Topos on one machine first, start with [Running Topos locally](local.md).
Every setting is on [Configuration](configuration.md), and this page
names a setting only to say what it is for.

## The image

The release image is `ghcr.io/latere-ai/topos`. A release tags it with the
release tag, such as `v0.70.0`, and with no other tag, so there is no
`latest` to follow: pin the tag you have read the
[CHANGELOG](../CHANGELOG.md) entry of, or its digest. The tagging is the
shared release stage's, in the [latere-ai/ci](https://github.com/latere-ai/ci)
repository.

The image is signed and carries a build attestation. To verify a tag
before you run it, follow
[Verifying a release](../SECURITY.md#verifying-a-release).

What the image holds:

- `/usr/local/bin/toposd`, its entrypoint. With no argument it runs the
  `serve` role.
- The machine helpers a Cella sandbox receives, in `/usr/local/lib/topos`,
  one static build per platform a sandbox may run on, a person's own
  computer included.
- Nothing else. The runtime is a distroless static image that runs as the
  non-root user, and it exposes ports 8080 and 8081. It has no shell and no
  `git`, so a runner that drives `git` or runs a session on its own host
  (`TOPOS_HOST_SESSIONS`) needs an image you build with those tools around
  `toposd`.

To build the image yourself, run `docker build .` in a checkout. The
`Dockerfile` compiles `toposd` and the helpers inside the build, and takes
`VERSION`, `COMMIT` and `DATE` build arguments that `toposd -version` and
`GET /version` report.

## The roles

One binary has four roles, chosen by its first argument that is not a flag.
[Configuration](configuration.md#roles) lists what each does and the exit
codes.

| Role | Run it to | Reads |
|---|---|---|
| `toposd serve` | serve the API, keep the store, run hosted sessions itself, end expired sessions and fire triggers | the settings of every area |
| `toposd runner` | drive hosted sessions on a machine of its own, claimed from a server | the model and machine settings, `TOPOS_INTERNAL_URL` and `TOPOS_RUNNER_TOKEN` |
| `toposd token` | print a token signed with the local issuer's key | `TOPOS_PUBLIC_URL`, `TOPOS_LOCAL_ISSUER_KEY` and `TOPOS_OIDC_AUDIENCE`; it opens no store |
| `toposd check` | nothing yet: it exits 1 and says it is not built | nothing |

A server drives hosted sessions in its own process, so a single `serve`
installation is complete. Add `toposd runner` replicas when sessions
should run apart from the API. A runner keeps nothing of its own: it
claims sessions from the server's internal listener, which answers
runners only when `TOPOS_RUNNER_TOKEN` is set on the server. A server and
its runners must be configured with the same `TOPOS_DENIALS_*` values.

A start-up with a missing or malformed setting exits 1 with one line that
names every problem, so the first start of a new installation reports
everything it lacks at once.

## The database

`serve` keeps agents, sessions and the session logs in one of two stores.

| Store | Chosen by | Replicas |
|---|---|---|
| Postgres | `TOPOS_DB_URL` | any number: a replica holds no session state, a session's log is the database's and its lease the queue's, and replicas tell each other of new events through Postgres notifications |
| The data directory | `TOPOS_DB_URL` unset | one: a serving `toposd` takes a lock on `TOPOS_DATA_DIR`, and a second `serve` on the same directory refuses to start and names the process that holds it |

Use Postgres for anything that runs more than one replica or must survive
the loss of the host's disk. The data directory is for one machine.

Postgres notes:

- `TOPOS_DB_URL` must reach the database directly. The schema migrations
  and the change notifications always use it. `TOPOS_DB_POOL_URL`, for a
  transaction-pooling proxy, serves queries only.
- The tests of the Postgres store run against Postgres 17. `toposd` states
  no other minimum, and its migrations create no extension.
- The database needs no setup but an empty database and a role that may
  create tables and indexes in it.

### Migrations

`toposd serve` migrates the schema itself, before it opens its listeners:
each start applies every migration in the binary that the database has not
had. There is no separate migrate command.

- Migrations only go forward. The `.down.sql` files in
  `internal/store/postgres/migrations` are not run by any role.
- Replicas that start together take a database lock, so one applies a
  migration and the others wait for it.
- A migration that was interrupted leaves the schema marked dirty, and the
  next start is refused with an error that names the version. The
  Deployment's start probe waits long enough for a slow migration for this
  reason: see [The manifests](#the-manifests).
- A binary older than the schema refuses to start with an error that says
  the database is at a higher version than the migrations it carries.
  Rolling back to an earlier release therefore needs a restore of a
  backup, or the down migration of each release you leave.
- Some releases also index existing data in the background after the
  start. Their CHANGELOG entry says so.

Take a backup before an upgrade that carries a migration.

### The data directory and the blob store

`TOPOS_DATA_DIR` also holds files that a server on Postgres writes: the
session directories of `TOPOS_HOST_SESSIONS` and a spool for the files a
message brings from a person's own files, which the server removes once the
message is kept or refused. The directory must be writable, and on a
read-only root file system it is the one place `serve` writes, so mount a
volume there. Nothing in it needs to outlive the process on a Postgres
installation without host sessions.

`TOPOS_BLOB_URL` moves raw model responses and captured requests out of the
database or the data directory, into a directory (`file://`) or an
S3 compatible object store (`s3://`). Leave it unset to keep them in the
store.

## Keys

| Key | Setting | What to know |
|---|---|---|
| Issuers whose tokens the server accepts | `TOPOS_OIDC_ISSUERS`, `TOPOS_OIDC_AUDIENCE`, `TOPOS_OIDC_INSECURE_ISSUERS` | The server reads every listed issuer's key set at start, and an issuer that does not answer, or whose discovery document names another issuer, stops the start. Tokens carry an audience from the list. An issuer on `http://` needs consent on a host other than loopback. |
| The local issuer's key | `TOPOS_LOCAL_ISSUER_KEY` | A PEM PKCS#8 private key, ECDSA P-256 or RSA of 2048 bits or more. The server signs nothing with it but the tokens of `toposd token`, names itself `TOPOS_PUBLIC_URL` as the issuer, and serves the public half at `<TOPOS_PUBLIC_URL>/.well-known/jwks.json`. It holds one key, so replacing it ends every token the old key signed. Give every replica the same key. |
| Runner bearers | `TOPOS_RUNNER_TOKEN` | A comma-separated list. The server accepts every bearer on it, and `toposd runner` sends the first. To rotate, add the new bearer to the server's list, roll the runners onto the new bearer, then remove the old one. |
| The authorizer's bearer | `TOPOS_AUTHORIZER_TOKEN` | The bearer the server sends to the authorizer. |
| Model credential | `TOPOS_MODELS_KEY` | Sent with `TOPOS_MODELS_URL`; refused beside session keys, whose sessions use keys of their own. |
| Object store | `TOPOS_BLOB_ACCESS_KEY`, `TOPOS_BLOB_SECRET_KEY` | Required with an `s3://` URL. There is no credential chain, so the server never reads the cloud provider's own credentials. |
| Credentials of other services | `TOPOS_CELLA_TOKEN_FILE`, `TOPOS_ORIGO_TOKEN_FILE`, `TOPOS_IDENTITY_SECRET_FILE`, `TOPOS_DECISIONS_TOKEN`, `TOPOS_SEARCH_KEY`, `TOPOS_SESSION_KEYS_TOKEN` | The `_FILE` settings name a file that the server reads again each time it uses the credential, so whatever renews a credential rewrites the file in place and nothing restarts. |

Keep the values in a Secret, not in a ConfigMap or the image. The base
Deployment reads an optional Secret named `toposd` beside the ConfigMap of
the same name. For an installation without an identity provider, see
[Running without an identity provider](configuration.md#running-without-an-identity-provider);
what each key lets a holder do is on [Security](security.md).

## Probes

Both listeners of `serve` answer three paths with no token, and so does the
one listener of `runner`:

| Path | Answers |
|---|---|
| `/livez` | `200` while the process runs |
| `/readyz` | `200` when the process is ready for traffic, `503` while it drains for a stop and, on a server, when the store does not answer |
| `/version` | `200` with the build's `version`, `commit` and `build_time` as JSON |

`TOPOS_PUBLIC_ADDR` is the public listener, the one an ingress forwards to.
`TOPOS_INTERNAL_ADDR` is the cluster's: the kubelet's probes, a metrics
scrape and, with `TOPOS_RUNNER_TOKEN` set, the routes of runners under
`/internal/v1`. Do not route the internal listener to the internet. A runner
serves only the three paths, on its internal address.

Also on the public listener, with no token:

- `GET /` answers the build identity as text.
- `GET <base>/openapi.yaml` answers the API document; see
  [The API](api.md#the-reference).
- With `TOPOS_BASE_PATH` set, `GET <base>/version` answers the same body as
  `/version`, so an ingress that forwards only the base path can still show
  which release runs. The probes stay at the listener's root.
- With a local issuer, the key set at `<TOPOS_PUBLIC_URL>/.well-known/jwks.json`.

On a stop signal, `readyz` answers `503` at once, the server waits briefly
so a load balancer notices, and it then closes its listeners and lets open
requests and streams end before it exits. Give the pod a termination grace
period longer than that; the base Deployment's is in its manifest.

## The manifests

The repository ships a Kustomize base and two directories around it. They
are read-only references for what to apply; an installation of your own
adds an overlay.

| Directory | Holds |
|---|---|
| `deploy/bootstrap` | `namespace.yaml`, the namespace `topos`, applied once by an operator |
| `deploy/base` | the server's Deployment, Service, ServiceAccount, NetworkPolicy and PodDisruptionBudget |
| `deploy/examples/generic` | the smallest overlay: the base in the `topos` namespace, an `images` entry for the image, and a ConfigMap generator for `TOPOS_PUBLIC_URL` and `TOPOS_OIDC_ISSUERS` |

What the base fixes, so an overlay does not repeat it:

- The container runs `serve`, as a non-root user with a read-only root file
  system, no added capabilities and the runtime default seccomp profile,
  and the pod mounts no Kubernetes API token.
- `envFrom` reads the ConfigMap `toposd` and, if it exists, the Secret
  `toposd`. The Deployment sets `TOPOS_OIDC_AUDIENCE` and the two
  listener addresses itself.
- The kubelet probes `/readyz` and `/livez` on the internal port. A start
  probe holds the liveness probe off for a long window so that a start
  that is still migrating is not killed, which would leave the migration
  dirty.
- The Service routes the public port only. The NetworkPolicy admits both
  ports and says nothing of outbound traffic.
- Two replicas, and a budget that lets one be down at a time.

What an overlay must add, because the base and the example do not:

1. The image: an `images` entry that points the placeholder name `toposd`
   at `ghcr.io/latere-ai/topos` and the tag or digest you pinned. The
   example's `ghcr.io/example/toposd:dev` is a stand-in.
2. The settings the server requires: `TOPOS_PUBLIC_URL`, an issuer
   (`TOPOS_OIDC_ISSUERS`, or `TOPOS_LOCAL_ISSUER_KEY` in the Secret), and
   `TOPOS_MODELS_URL`. The example's ConfigMap sets the first two only.
3. A store. With Postgres, set `TOPOS_DB_URL` in the Secret. Without it,
   run one replica, not two, and give it a persistent volume at
   `TOPOS_DATA_DIR`.
4. A writable `TOPOS_DATA_DIR`, for the reason above: on the base's
   read-only root file system, mount an `emptyDir` or a volume there.
5. Outbound rules, if you restrict egress: the server reaches the issuer,
   the authorizer, the database, the model connection and each service
   you configure.
6. An ingress to the Service's public port, with TLS. `TOPOS_PUBLIC_URL` is
   the address clients reach it at.
7. For separate runners, a Deployment of the same image with the argument
   `runner`, a Service for the server's internal port, and
   `TOPOS_INTERNAL_URL` and `TOPOS_RUNNER_TOKEN` for the runners. The base
   has no runner Deployment.

## Upgrading

An upgrade is a new image tag rolled over the Deployment.

1. Read the [CHANGELOG](../CHANGELOG.md) entry of every release between the
   one you run and the one you pull. An entry that needs an operator to
   act says so, under `Changed`: an order to roll in, a setting to add, a
   migration that takes time. The `Changed` and `Removed` lists also hold
   what stops working.
2. Back up the database if any entry carries a migration.
3. Roll the server. Replicas are interchangeable, so a rolling update keeps
   the API up. The first replica of the new release migrates the schema
   and the others wait for it.
4. Roll the runners onto the same release. A runner that does not know an
   event type a newer server wrote stops that session with
   `schema_too_new`, and leases move sessions between runners during a
   roll, so roll runners and server together unless an entry says an
   order.
5. Roll clients last when an entry says so: an older server refuses
   request members it does not know.
6. Check `GET /version` against the tag you rolled.

The data directory has no migration step. A release that has to rewrite
stored files does it when it opens the directory, and says so in its entry.

A release can add an event type or a request member that an authorizer must
understand. The authorizer's contract is on [Authorizer](authorizer.md).

## What a production installation needs

`toposd serve` starts with an issuer and a model connection. A useful
installation has more around it. Each row names the setting that connects
it and the page that covers it.

| Needs | Why | Settings | Covered on |
|---|---|---|---|
| An issuer | every request carries a bearer, and there is no anonymous access | `TOPOS_OIDC_ISSUERS`, `TOPOS_OIDC_AUDIENCE`, or `TOPOS_LOCAL_ISSUER_KEY` | [Configuration](configuration.md#running-without-an-identity-provider), [Authenticating](api.md#authentication) |
| An authorizer | decides each action of each caller; without one, the owner policy lets a subject act on its own objects and the subjects of `TOPOS_ADMIN_SUBJECTS` on every object | `TOPOS_AUTHORIZER_URL`, `TOPOS_AUTHORIZER_TOKEN` | [Authorizer](authorizer.md), [Security](security.md) |
| A model connection | an agent that names no connection runs on it: a Lux gateway or a provider's API | `TOPOS_MODELS_URL`, `TOPOS_MODELS_KEY` | [Configuration](configuration.md) |
| Cella, for hosted machines | a hosted session's tools act in a sandbox that Cella creates | `TOPOS_CELLA_URL`, `TOPOS_CELLA_TOKEN_FILE`, `TOPOS_CELLA_LABELS` | [Configuration](configuration.md), [A session's network](network.md) |
| Postgres | more than one replica, and logs that outlive a disk | `TOPOS_DB_URL`, `TOPOS_DB_POOL_URL` | [The database](#the-database) |
| An object store | raw model responses and captured requests kept outside the database | `TOPOS_BLOB_URL` and its keys | [The data directory and the blob store](#the-data-directory-and-the-blob-store) |
| An identity provider for agents | each agent gets an identity of its own, and each session reaches other services with short tokens | `TOPOS_IDENTITY_URL`, `TOPOS_IDENTITY_CLIENT_ID`, `TOPOS_IDENTITY_SECRET_FILE` | [Configuration](configuration.md) |
| Session keys | each hosted session asks models with a key of its own, which the authorizer's key routes register | `TOPOS_SESSION_KEYS_URL`, `TOPOS_SESSION_KEYS_TOKEN` | [Configuration](configuration.md) |
| A files service | files people attach and files an answer shows are kept with the person's own files, and a session's working directory survives its sandbox | `TOPOS_FILES_AUDIENCE` | [Reading a session's files](reading-files.md), [Memory folders](memory-folders.md) |
| A git host | the sandbox clones from it, and a session's checkpoints live there | `TOPOS_ORIGO_URL`, `TOPOS_ORIGO_TOKEN_FILE` | [Configuration](configuration.md) |
| An app host | the `publish` tool puts a session's folder online | `TOPOS_APPS_URL`, `TOPOS_APPS_AUDIENCE` | [Publishing](publishing.md#connecting-an-app-host) |
| A connection service | people's connections to other services, reached without their credentials in the sandbox | `TOPOS_CONNECTIONS_URL`, `TOPOS_CONNECTIONS_AUDIENCE` | [Connections](connections.md) |
| A search service | the `web_search` tool | `TOPOS_SEARCH_URL`, `TOPOS_SEARCH_KEY` | [Web search](web-search.md) |
| A parsing service | the `read` tool reads a PDF as text | `TOPOS_PARSING_URL` | [Configuration](configuration.md) |
| A decision service | suggests a verdict for each tool call of a hosted session | `TOPOS_DECISIONS_URL` and the other `TOPOS_DECISIONS_*` settings | [How a call was decided](how-a-call-was-decided.md) |
| Telemetry | traces, metrics and logs exported over OTLP | `OTEL_EXPORTER_OTLP_ENDPOINT`; unset, nothing is exported and logs go to standard error | the OpenTelemetry SDK's own documentation |

A hosted session whose tools act on a machine needs Cella, or
`TOPOS_HOST_SESSIONS` to run them on the server's own host. Without either,
the call answers `machine_unavailable`. How the two settings combine for an
agent that names no machine is in the `TOPOS_HOST_SESSIONS` and
`TOPOS_CELLA_URL` rows of [Configuration](configuration.md); what running
on the server's host exposes is on [Security](security.md).
