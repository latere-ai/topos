# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0

# The verification contract for topos.
#
# Every target here is one latere-ai/ci's go-verify workflow probes for and
# runs, so `make <target>` on a laptop is the same check the runner performs.
# The gates themselves live in latere.ai/x/ci-gate, pinned in go.mod; what
# each one asserts for this repository is in .lateregate.yaml.

.PHONY: all check build test test-race test-hermetic cover fmt fmt-check lint lint-config lint-modernize spec-lint validate vuln tidy hooks

all: fmt-check lint test cover spec-lint validate

build:
	go build ./...

# vet before test, because a vet finding is a fact about the code that does
# not need the suite to run to be true.
test:
	@go tool lateregate test

# The suite under the race detector. Kept separate from `test` so the fast
# path stays fast and a race failure names itself.
test-race:
	@go tool lateregate race

# The suite with only the Go toolchain and the directories .lateregate.yaml
# names on PATH. A test that depends on whatever happens to be installed
# passes locally and fails on a runner, which is the worst order to find out.
test-hermetic:
	@go tool lateregate hermetic

# The floor lives in this target rather than a separate one: CI runs
# `make cover`, and a target that only prints a percentage reports green at
# any coverage. The examples/ packages are runnable demonstrations with no
# tests; they compile and run here but are filtered out of the measurement so
# demo code does not dilute the production total.
cover:
	@go tool lateregate cover

fmt:
	gofmt -w .

fmt-check:
	@go tool lateregate fmt-check

# Fails on code a standard library call or a language builtin already covers.
# Carries fixers golangci-lint's modernize linter does not, so it runs whether
# or not the linter does.
lint-modernize:
	@go tool lateregate modernize

# .golangci.yml is generated and gitignored: golangci-lint has no config
# inheritance, so the org's set is rendered from latere.ai/x/ci-gate on every
# run. Regenerating is what makes divergence impossible rather than merely
# detectable.
lint-config:
	@go tool lateregate golangci

# golangci-lint at the version lateregate pins, against the config it renders.
lint:
	@go tool lateregate lint

# specs/ records the shipped surface, and a spec tree nobody checks drifts
# from the code within a milestone. The vocabulary and the required
# frontmatter are in .lateregate.yaml.
spec-lint:
	@go tool lateregate spec-lint

# The repo-specific check the shared pipeline cannot know about.
validate: vuln

# A dependency with a known advisory is a fact about the module graph, not
# about this code, so nothing else here would ever report it.
vuln:
	@go tool lateregate vuln

tidy:
	go mod tidy

# hooks installs the repository git hooks (pre-commit gofmt and go fix guards).
hooks:
	git config core.hooksPath .githooks
	@[ -e CLAUDE.md ] || [ -L CLAUDE.md ] || ln -s AGENTS.md CLAUDE.md
	@echo "installed git hooks (core.hooksPath=.githooks)"

# The whole shared bar. Every gate lives in lateregate, pinned as a tool in
# go.mod; this target is a name for `go tool lateregate` and nothing else.
# The plan: `go tool lateregate list`. One gate: `go tool lateregate <gate>`.
check:
	@go tool lateregate
