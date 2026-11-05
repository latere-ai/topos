#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0
#
# Post-deploy live check of a running Topos. The release pipeline of the
# platform runs it after the rollout and appends its Markdown evidence to
# the release; an operator runs it by hand after an upgrade.
#
# It is unauthenticated, read-only and safe to run any number of times. It
# does not wait: it fails at once and the caller runs it again.
#
# Required tools: curl, grep.
#
# Environment:
#   BASE_URL   the API root of the installation, with its base path, for
#              example https://api.example.com/v1/agents; required
#   TAG        the release being proved; see the gap below
#   COMMIT     the release commit, for the evidence
#   DEPLOY_URL the run that deployed it, for the evidence
#   OUTPUT_MD  where to write the Markdown evidence; nowhere when unset
#
# What is public: GET ${BASE_URL}/openapi.yaml answers without a token, so
# that is the check that the route reaches toposd. Probes /livez, /readyz
# and /version are served by toposd at the origin root only; the ingress
# claims the base path alone, so no health route and no version route is
# public. The served version therefore cannot be compared with TAG, and
# the evidence says so. The missing route is ${BASE_URL}/version.

set -euo pipefail

BASE_URL="${BASE_URL:-}"
BASE_URL="${BASE_URL%/}"
TAG="${TAG:-}"
COMMIT="${COMMIT:-unknown}"
DEPLOY_URL="${DEPLOY_URL:-}"
OUTPUT_MD="${OUTPUT_MD:-}"

pass() { printf 'ok: %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

[ -n "$BASE_URL" ] || fail "BASE_URL is required; this smoke has no default target"
for cmd in curl grep; do
  command -v "$cmd" >/dev/null || fail "$cmd is required"
done

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

code=$(curl -sS -o "$tmp/openapi" -w '%{http_code}' "${BASE_URL}/openapi.yaml") \
  || fail "GET /openapi.yaml: curl failed"
[ "$code" = "200" ] || fail "GET /openapi.yaml: expected 200, got $code (body: $(head -c 200 "$tmp/openapi" 2>/dev/null || true))"
grep -q '^openapi: ' "$tmp/openapi" || fail "GET /openapi.yaml: the body is not an OpenAPI description"
pass "GET /openapi.yaml ($code)"

if [ -n "$TAG" ]; then
  pass "served version not publicly readable; $TAG not compared"
fi

if [ -n "$OUTPUT_MD" ]; then
  {
    echo "<!-- release-evidence -->"
    echo
    echo "## Release evidence"
    echo
    echo "- Tag: \`${TAG:-unknown}\`"
    echo "- Commit: \`${COMMIT}\`"
    [ -n "$DEPLOY_URL" ] && echo "- Deploy: ${DEPLOY_URL}"
    echo "- Origin: ${BASE_URL}"
    echo "- Smoke: \`/openapi.yaml\` answered 200 with an OpenAPI description"
    echo "- Served version: not publicly readable, so not compared with the tag"
  } > "$OUTPUT_MD"
fi

printf '\nrelease smoke passed\n'
