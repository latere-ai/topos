# Security

Topos runs model-driven tool calls against real machines, with
credentials in its reach. This file is for someone who found a
vulnerability and for someone deciding whether to trust Topos: what it
commits to, the test that holds each commitment, and what it leaves to
whoever runs it.

## Reporting a vulnerability

Report one privately, through
[GitHub's private vulnerability reporting](https://github.com/latere-ai/topos/security/advisories/new)
or by email to security@latere.ai. Do not open a public issue for it. The
[organization's security policy](https://github.com/latere-ai/.github/blob/main/SECURITY.md)
sets the response times, how disclosure works, and the safe harbor for
research done in good faith. Credit in the release notes on request.

Fixes ship in the next release of the current major line, as the
organization's policy on
[supported versions](https://github.com/latere-ai/.github/blob/main/SECURITY.md#supported-versions)
says; the [releases page](https://github.com/latere-ai/topos/releases)
lists them.

The reports we most want: an API call that acts without the authorizer's
allow, or that the authorizer's refusal or absence lets through; a
credential's value in a session's log, a blob, an event or a sandbox; a
command of one session that reads another session's files or the
server's; and anything that lets a session act with more than its
session's permissions. What [is not defended](#what-is-not-defended) is
not a vulnerability, but a report that one of those limits is wider than
written here is.

## What is protected

Topos is attacked from four places: a caller of the API, the model of a
session steered by what it reads, the code a session runs on its
machine, and a runner or another session on the same server. Each
commitment below is held by the named test, which runs on every change.

| Commitment | Held by |
|---|---|
| A token is refused when it comes from an issuer `TOPOS_OIDC_ISSUERS` does not list, names a wrong audience, is badly signed, has no `iat`, or is older than the age bound. | `TestVerificationRules` in `internal/auth` |
| Every action a caller takes is decided by the installation's authorizer, or by the built-in owner policy when none is configured. When the authorizer cannot be reached, every route that asks it answers `authorizer_unavailable` and nothing is written. | `TestAuthorizerDownIsRefusal` in `internal/server`, `TestAuthorizerConformanceOwnerPolicy` in `internal/auth` |
| The server refuses to start when `TOPOS_AUTHORIZER_URL` or `TOPOS_CELLA_URL` is plain `http` on a host other than loopback, which would send its bearer in the clear, unless the operator consents with `TOPOS_AUTHORIZER_INSECURE` or `TOPOS_CELLA_INSECURE`. | `TestAnHTTPAuthorizerNeedsConsent`, `TestTheRunnerVariables` in `internal/config` |
| The routes runners claim sessions from answer only a bearer that `TOPOS_RUNNER_TOKEN` lists. | `TestTheRoutesNeedTheRunnerToken` in `internal/runnerrole` |
| A session has one writer. Once another holder takes an expired lease, an append through the old lease is refused and writes nothing. | `TestAFencedAppendAfterATakeoverIsRefused`, `TestPostgresTwoReplicasOneWriter` in `internal/store/postgres` |
| The model connection's credential reaches the model gateway in a header, and no event and no blob of the session holds it. | `TestModelCredentialNeverLogged` in `harness` |
| A device machine, a person's own computer reached through Cella, opens with no Secret mount, no `LUX_URL` and no variable of the runner's. A rollout's run mounts no Secret of the connection service. | `TestDeviceOpensWithNoSecrets`, `TestARunsSandboxMountsNoConnectionsSecret` in `internal/hosted` |
| An agent manifest that holds a credential in any free-text field is refused, and the refusal does not quote it. | `TestManifestHoldingASecretIsRefused` in `manifest` |
| A subagent acts with its session's credentials. The identity and permissions its own agent declares are ignored, and the session records what was ignored. A subagent whose agent names a stricter approval mode keeps it, and a thread a spawn starts holds no `publish` tool. | `TestASubagentActsWithTheSessionsCredentials`, `TestAThreadKeepsItsAgentsStricterMode`, `TestASpawnedThreadHoldsNoPublish` in `harness` |
| On a host machine, the file tools reach only the session's roots, through no `..` and no symbolic link out of them, and refuse files on the deny list, such as `.env`, inside a root. | `TestFilesAreConfinedToTheRoots`, `TestTheDenyListHoldsInsideARoot` in `machine/host` |
| With `TOPOS_HOST_SESSIONS=on`, the server and the runner refuse to start when the host sandbox is missing, cannot run a probe command, or lets the probe read the data directory. | `TestHostSessionsOnAServer` in `cmd/toposd` |
| A sandboxed host fetch reaches only its egress hosts, every redirect included. | `TestASandboxedFetchReachesOnlyTheEgress` in `machine/host` |
| The server roles refuse to start on the scripted model, so a served session never answers from a script. | `TestServeRefusesScriptedModel` in `cmd/toposd` |
| A refusal for spend, from the model gateway or from Cella, stops the turn with the stop reason `budget`. The gateway's is asked once, not retried as a rate limit is. | `TestASpentBudgetAtTheGatewayStopsTheTurnWithBudget` in `harness`, `TestACoresSpendRefusalStopsTheTurn` in `cmd/toposd` |

[docs/security.md](docs/security.md) says, for an operator and a builder,
what Topos sees and keeps, and what an operator must keep safe.

## What is not defended

- **A model steered by what it reads.** Topos does not detect prompt
  injection in a file, a web page or a tool's result. A steered model can
  do whatever its session may do: the session's approval mode, its
  machine and the commitments above bound it, nothing else.
- **The code a session runs in its Cella sandbox.** A sandbox mounts the
  session's credentials as Cella Secrets: its Lux key with session keys,
  its git host credential, and its token for the connection service.
  Cella's egress gateway, not Topos, keeps their values out of the
  sandbox and sends each only to its own host. Within that, code in the
  sandbox can spend the session's model allowance and act on the git host
  and the connected services with the session's rights while the sandbox
  exists.
- **Commands on a host machine with no host sandbox.** The roots and the
  deny list bind the file tools, not commands. A command `bash` runs on a
  person's own computer without the host sandbox runs as the person's
  account, and the approval mode is what stands between the model and it.
- **The host sandbox's confinement itself.** Which files and variables a
  host session's command can read is `srt`'s to enforce.
  `TestHostSessionsAreConfined` checks it only on a machine with `srt`
  installed, and the checks that run on every change do not install it.
- **A runner's host.** A runner holds the short-lived tokens of the
  sessions it drives at that moment. Whoever controls its host acts as
  those sessions until their tokens expire.
- **What a session sends to its model.** Every message, tool result and
  file a session reads reaches the model's provider. A secret a person
  sends in a message has reached the provider, and Topos cannot take it
  back.
- **The services Topos trusts.** Topos takes the answers of the identity
  provider, the authorizer, Lux, Cella and the connection service as
  given: an issuer that signs for the wrong person, or an authorizer that
  allows too much, is outside what Topos can catch. A sandbox escape from
  Cella is Cella's to defend.
- **Whoever runs the server.** The operator holds the local issuer key,
  the database, the blob store, the runner tokens and every credential
  file in the configuration, and can read every session.

## Audit

Topos has had no external security audit. The commitments above are held
by tests in this repository, which run on every change.

## Supply chain

Dependencies are checked for known vulnerabilities on every push. A
release is built from a tagged commit whose checks passed, through the
shared release stages in [latere-ai/ci](https://github.com/latere-ai/ci).
It carries the `toposd` and `topos-machine` binaries, an SPDX bill of
materials for the `topos` image, `checksums.txt` over every file of the
release, cosign signatures over the image and over `checksums.txt`, and a
build provenance attestation for the image. The signatures name the stage
that made them, and the commands below hold them to this repository.

### Verifying a release

With `TAG` the release's tag, such as `vX.Y.Z`:

```sh
gh release download "$TAG" -R latere-ai/topos -D topos-release
cd topos-release
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/latere-ai/ci/\.github/workflows/image-publish\.yml@' \
  --certificate-github-workflow-repository latere-ai/topos checksums.txt
sha256sum -c checksums.txt

cosign verify "ghcr.io/latere-ai/topos:$TAG" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/latere-ai/ci/\.github/workflows/image-build\.yml@' \
  --certificate-github-workflow-repository latere-ai/topos
gh attestation verify "oci://ghcr.io/latere-ai/topos:$TAG" -R latere-ai/topos \
  --signer-workflow latere-ai/ci/.github/workflows/image-build.yml
```

The release workflow runs the same checks against every release it
publishes. Releases before v0.64.1 carry no signature.
