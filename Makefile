GOLANGCI_LINT_VERSION := v2.12.2
BIN_DIR := $(CURDIR)/bin
# Version-addressed binary: bumping GOLANGCI_LINT_VERSION invalidates the
# target (and any CI cache of bin/) instead of silently reusing an old binary.
GOLANGCI_LINT := $(BIN_DIR)/golangci-lint-$(GOLANGCI_LINT_VERSION)

COMPOSE_FILE := conformance/docker-compose.yml

.PHONY: build test test-integration lint conformance conformance-live conformance-chaos load-test dev dev-down

build:
	go build ./...

test:
	go test ./...

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run

$(GOLANGCI_LINT):
	GOBIN=$(BIN_DIR) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	mv $(BIN_DIR)/golangci-lint $(GOLANGCI_LINT)

INT_COMPOSE := docker compose -f $(COMPOSE_FILE) -f conformance/compose.integration.yml -p registry-int
INT_ENV := S3_TEST_ENDPOINT=127.0.0.1:19000 \
	S3_TEST_ACCESS_KEY=registry S3_TEST_SECRET_KEY=registry-secret \
	PG_TEST_DSN=postgres://registry:registry@127.0.0.1:15432/registry

# Integration tests (build tag "integration") against MinIO and Postgres
# from the compose stack.
test-integration:
	$(INT_COMPOSE) up -d --wait minio postgres
	$(INT_ENV) go test -tags integration -count=1 ./...; status=$$?; \
		$(INT_COMPOSE) down -v; exit $$status

conformance:
	./conformance/run.sh

# Chaos scenarios: two replicas, injected failures (kill -9, postgres down,
# upstream down). Separate target: slower and deliberately destructive.
conformance-chaos:
	./conformance/run-chaos.sh

# k6 load test ("CI storm") against a warm cache; writes docs/perf.md.
load-test:
	./conformance/run-load.sh

# Conformance against real upstreams (Maven Central, registry.terraform.io).
# Manual run: needs internet access.
conformance-live:
	./conformance/run-live.sh

DEV_COMPOSE := docker compose -f $(COMPOSE_FILE) -f conformance/compose.dev.yml -p registry-dev

dev:
	$(DEV_COMPOSE) up -d --wait minio postgres
	go run ./cmd/registry -config conformance/dev.yaml

dev-down:
	$(DEV_COMPOSE) down -v
