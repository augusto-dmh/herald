# Single source of truth for verification commands.
# CI and the AI harness reference these target NAMES; change commands only here.

GOLANGCI_LINT_VERSION := v2.12.2
GOVULNCHECK_VERSION   := v1.6.0

.PHONY: build gate-quick gate-integration gate-full lint vulncheck

build:
	go build ./...
	go vet ./...

gate-quick:
	go test -race ./...

# Integration tests use testcontainers; make sure Docker is running first.
gate-integration:
	go test -race -tags=integration ./...

gate-full: gate-quick gate-integration

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...
