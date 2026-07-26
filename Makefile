GOLANGCI_LINT_VERSION := v2.12.2
BIN_DIR := $(CURDIR)/bin
# Version-addressed binary: bumping GOLANGCI_LINT_VERSION invalidates the
# target (and any CI cache of bin/) instead of silently reusing an old binary.
GOLANGCI_LINT := $(BIN_DIR)/golangci-lint-$(GOLANGCI_LINT_VERSION)

COMPOSE_FILE := conformance/docker-compose.yml

.PHONY: build test lint conformance conformance-live dev dev-down

build:
	go build ./...

test:
	go test ./...

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run

$(GOLANGCI_LINT):
	GOBIN=$(BIN_DIR) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	mv $(BIN_DIR)/golangci-lint $(GOLANGCI_LINT)

conformance:
	./conformance/run.sh

# Runs conformance scenarios against real upstreams (Maven Central, npmjs, ...).
# No live scenarios exist yet; they arrive with the first real format modules
# (explicit task in Phase 2 of PLAN.md).
conformance-live:
	@echo "conformance-live: no live scenarios yet (planned for Phase 2)" >&2; exit 2

DEV_COMPOSE := docker compose -f $(COMPOSE_FILE) -f conformance/compose.dev.yml -p registry-dev

dev:
	$(DEV_COMPOSE) up -d --wait minio postgres
	go run ./cmd/registry -config conformance/dev.yaml

dev-down:
	$(DEV_COMPOSE) down -v
