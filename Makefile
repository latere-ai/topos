GOLANGCI_LINT ?= golangci-lint
COVER_MIN ?= 90

.PHONY: all lint vet test cover cover-check vuln tidy

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
cover-check: cover
	@total=$$(go tool cover -func=coverage.out | awk '/^total:/ {print substr($$3, 1, length($$3)-1)}'); \
	echo "total coverage: $$total% (min $(COVER_MIN)%)"; \
	awk "BEGIN { exit !($$total >= $(COVER_MIN)) }" || { echo "coverage below $(COVER_MIN)%"; exit 1; }

# vuln runs the Go vulnerability scanner.
vuln:
	govulncheck ./...

tidy:
	go mod tidy
