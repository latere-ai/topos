# The verification contract for topos.
#
# Every target here is one latere-ai/ci's go-verify workflow probes for and
# runs, so `make <target>` on a laptop is the same check the runner performs.
# The gates themselves live in latere.ai/x/ci-gate, pinned in go.mod; what
# each one asserts for this repository is in .lateregate.yaml.

COVER_MIN ?= 90

.PHONY: all build test test-race test-hermetic cover fmt fmt-check lint lint-config lint-modernize spec-lint validate vuln tidy hooks

all: fmt-check lint test cover spec-lint validate

build:
	go build ./...

# vet before test, because a vet finding is a fact about the code that does
# not need the suite to run to be true.
test:
	go vet ./...
	go test ./...

# The suite under the race detector. Kept separate from `test` so the fast
# path stays fast and a race failure names itself.
test-race:
	go test -race -timeout 120s ./...

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
	go test -race -coverprofile=coverage.out -timeout 120s ./...
	@grep -v '/examples/' coverage.out > coverage.gate.out; \
	total=$$(go tool cover -func=coverage.gate.out | awk '/^total:/ {print substr($$3, 1, length($$3)-1)}'); \
	echo "total coverage (excluding examples): $$total% (min $(COVER_MIN)%)"; \
	awk "BEGIN { exit !($$total >= $(COVER_MIN)) }" || { echo "coverage below $(COVER_MIN)%"; exit 1; }

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

GOLANGCI_VERSION ?= v2.13.1

# The linter CI runs, against the config lint-config renders. Without this the
# only machine that ever lints this repository is a runner.
lint: lint-config
	@go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION) run ./...

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
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

tidy:
	go mod tidy

# hooks installs the repository git hooks (pre-commit gofmt and go fix guards).
hooks:
	git config core.hooksPath .githooks
	@[ -e CLAUDE.md ] || [ -L CLAUDE.md ] || ln -s AGENTS.md CLAUDE.md
	@echo "installed git hooks (core.hooksPath=.githooks)"
