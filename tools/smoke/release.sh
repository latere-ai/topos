#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0
#
# Post-deploy live check of a running Topos. The release workflow's deploy
# job runs it after rolling the maintainer's own installation and appends
# its Markdown evidence to the release; an operator runs it by hand after an
# upgrade.
#
# It is unauthenticated, read-only and safe to run any number of times. It
# does not wait: it fails at once and the caller runs it again.
#
# Required tools: curl, grep.
#
# Environment:
#   BASE_URL   the API root of the installation, with its base path, for
#              example https://api.example.com/v1/agents; required
#   TAG        the release being proved; when set the served version must
#              equal it
#   COMMIT     the release commit, for the evidence
#   DEPLOY_URL the run that deployed it, for the evidence
#   OUTPUT_MD  where to write the Markdown evidence; nowhere when unset
#
# What is public: the ingress claims the base path alone. toposd serves
# /livez and /readyz at the origin root only, so there is no public health
# route. GET ${BASE_URL}/openapi.yaml answers without a token, so that is the
# check that the route reaches toposd, and GET ${BASE_URL}/version serves the
# build's version.
#
# Checks, in order, stopping at the first failure:
#   1. GET ${BASE_URL}/openapi.yaml answers 200 with an OpenAPI description.
#   2. GET ${BASE_URL}/version serves a version, and when TAG is set it
#      equals TAG.

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

code=$(curl -sS --max-time 15 -o "$tmp/version" -w '%{http_code}' "${BASE_URL}/version") \
  || fail "GET /version: curl failed"
[ "$code" = "200" ] || fail "GET /version: expected 200, got $code (body: $(head -c 200 "$tmp/version" 2>/dev/null || true))"
pass "GET /version ($code)"
# `|| true`: grep exits 1 on no match, and the message below should be said.
served=$(grep -o '"version":"[^"]*"' "$tmp/version" | head -1 | cut -d'"' -f4 || true)
[ -n "$served" ] || fail "GET /version: no version in $(head -c 200 "$tmp/version")"
if [ -n "$TAG" ]; then
  [ "$served" = "$TAG" ] || fail "version mismatch: served $served, expected $TAG"
  pass "served version matches the tag ($served)"
else
  pass "served version recorded ($served)"
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
    echo "- Served version: \`${served}\`${TAG:+, equal to the tag}"
  } > "$OUTPUT_MD"
fi

printf '\nrelease smoke passed\n'
