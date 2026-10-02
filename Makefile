# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0

GO ?= go

.PHONY: build check clean fmt hooks run token

# The whole bar. Every gate lives in latere.ai/x/ci-gate, pinned as a tool
# in go.mod and configured in .lateregate.yaml, so this target is a name for
# `go tool lateregate` and nothing else. One gate at a time: `go tool
# lateregate cover`. The plan: `go tool lateregate list`.
check:
	@$(GO) tool lateregate

.DEFAULT_GOAL := check

OUT_DIR := out
MODULE := $(shell $(GO) list -m)

# Build metadata, deferred so the git and date calls run only for a build. A
# dirty tree marks the commit, because a binary built from uncommitted
# changes cannot be reproduced from its commit.
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DIRTY = $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo -dirty)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION_PKG = $(MODULE)/internal/version
LDFLAGS = -X $(VERSION_PKG).Version=$(VERSION) \
          -X $(VERSION_PKG).Commit=$(COMMIT)$(DIRTY) \
          -X $(VERSION_PKG).Date=$(BUILD_DATE)

# Both binaries: toposd, the server, and topos, the scripting and test client.
build:
	@mkdir -p $(OUT_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(OUT_DIR)/toposd ./cmd/toposd
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(OUT_DIR)/topos ./cmd/topos
	@echo "built $(OUT_DIR)/toposd $(OUT_DIR)/topos"

# The server on loopback as a self-hoster runs it: the local issuer with a
# key generated once under out/, the owner policy, and admin as its admin.
# `make token` prints a token for it. TOPOS_MODELS_URL, and TOPOS_MODELS_KEY
# where the connection needs one, come from the environment: toposd does
# not start without a model connection.
LOCAL_KEY := $(OUT_DIR)/local-issuer.pem
LOCAL_ENV = TOPOS_PUBLIC_URL=http://127.0.0.1:8080 \
	TOPOS_LOCAL_ISSUER_KEY="$$(cat $(LOCAL_KEY))" \
	TOPOS_ADMIN_SUBJECTS='http://127.0.0.1:8080|admin'

$(LOCAL_KEY):
	@mkdir -p $(OUT_DIR)
	openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out $@

run: build $(LOCAL_KEY)
	$(LOCAL_ENV) TOPOS_PUBLIC_ADDR=127.0.0.1:8080 TOPOS_INTERNAL_ADDR=127.0.0.1:8081 \
		$(OUT_DIR)/toposd

token: build $(LOCAL_KEY)
	@$(LOCAL_ENV) $(OUT_DIR)/toposd token

fmt:
	gofmt -w $$(git ls-files '*.go')

# Point git at the delegating hooks. Per clone, so it is a target.
hooks:
	chmod +x .githooks/*
	git config core.hooksPath .githooks

# Remove the build output.
clean:
	rm -rf $(OUT_DIR)
