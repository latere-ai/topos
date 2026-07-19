GOLANGCI_LINT ?= golangci-lint
COVER_MIN ?= 90

.PHONY: all lint vet test cover cover-check vuln tidy fmt fmt-check hooks

all: lint vet test cover-check

# lint runs the formatters (gofmt + goimports) and the enabled linters.
lint:
	$(GOLANGCI_LINT) fmt --diff ./...
	$(GOLANGCI_LINT) run ./...

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

# hooks installs the repository git hooks (pre-commit gofmt guard).
hooks:
	git config core.hooksPath .githooks
	@echo "installed git hooks (core.hooksPath=.githooks)"
