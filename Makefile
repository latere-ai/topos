GOLANGCI_LINT ?= golangci-lint
COVER_MIN ?= 90

.PHONY: all lint lint-modernize vet test cover cover-check vuln tidy fmt fmt-check hooks

all: lint vet test cover-check

# lint runs the formatters (gofmt + goimports) and the enabled linters.
lint: lint-modernize
	$(GOLANGCI_LINT) fmt --diff ./...
	$(GOLANGCI_LINT) run ./...

# lint-modernize fails on code that a standard library call already covers.
# It runs the toolchain modernizers, which overlap golangci-lint's modernize
# linter but add three it does not carry: buildtag, hostport, and the
# go:fix inline directives. newexpr and errorsastype are off for the reasons
# recorded in .golangci.yml.
# Only a non-empty patch fails the target. go fix also exits non-zero when a
# package does not type-check, which is a build error rather than a finding,
# so stderr is dropped and the decision rests on the patch alone.
lint-modernize:
	@for fixer in newexpr errorsastype; do \
		go tool fix help 2>&1 | grep -q "^    $$fixer " || { \
			echo "go fix no longer carries the $$fixer fixer, so -$$fixer=false is rejected and this check passes silently"; \
			exit 1; \
		}; \
	done
	@patch=$$(go fix -diff -newexpr=false -errorsastype=false ./... 2>/dev/null); \
	if [ -n "$$patch" ]; then \
		echo "$$patch"; \
		echo "go fix: the diff above is already in the standard library; apply it with go fix"; \
		exit 1; \
	fi

vet:
	go vet ./...

# test runs the full suite under the race detector.
test:
	go test -race -timeout 120s ./...

# cover writes a coverage profile and prints the total.
cover:
	go test -race -coverprofile=coverage.out -timeout 120s ./...
	go tool cover -func=coverage.out | tail -1

# cover-check fails when total statement coverage is below COVER_MIN.
# The examples/ packages are runnable demonstrations with no tests;
# `cover` still compiles and runs them, but they are filtered out of the
# gate measurement so demo code does not dilute the production total.
cover-check: cover
	@grep -v '/examples/' coverage.out > coverage.gate.out; \
	total=$$(go tool cover -func=coverage.gate.out | awk '/^total:/ {print substr($$3, 1, length($$3)-1)}'); \
	echo "total coverage (excluding examples): $$total% (min $(COVER_MIN)%)"; \
	awk "BEGIN { exit !($$total >= $(COVER_MIN)) }" || { echo "coverage below $(COVER_MIN)%"; exit 1; }

# vuln runs the Go vulnerability scanner.
vuln:
	govulncheck ./...

tidy:
	go mod tidy

# fmt formats all Go sources in place.
fmt:
	gofmt -w .

# fmt-check fails if any Go source is not gofmt-formatted.
fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt: unformatted files:"; echo "$$out"; exit 1; fi

# hooks installs the repository git hooks (pre-commit gofmt and go fix guards).
hooks:
	git config core.hooksPath .githooks
	@echo "installed git hooks (core.hooksPath=.githooks)"
