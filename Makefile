GOLANGCI_LINT_VERSION := v2.12.2
BIN_DIR := $(CURDIR)/bin
# Version-addressed binary: bumping GOLANGCI_LINT_VERSION invalidates the
# target (and any CI cache of bin/) instead of silently reusing an old binary.
GOLANGCI_LINT := $(BIN_DIR)/golangci-lint-$(GOLANGCI_LINT_VERSION)

COMPOSE_FILE := conformance/docker-compose.yml

.PHONY: build ui test test-integration lint conformance conformance-live conformance-chaos conformance-geo terraform-build terraform-test terraform-docs load-test dev dev-down dev-ha dev-ha-down smoke

# `go build ./...` alone also works — the console directory carries a
# placeholder so the embed compiles — but the binary then reports the console
# as not built. `make build` is what produces a complete registry.
build: ui
	go build ./...

# The console. Its output is generated, so it is not committed; this is the
# one place that produces it.
ui:
	cd ui && npm ci && npm run build

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

# Two-site geo federation: replication, conflicts (rule K1), partition/heal.
conformance-geo:
	./conformance/geo/run.sh

# Terraform provider: its own Go module, so it builds and lints separately.
terraform-build: $(GOLANGCI_LINT)
	cd terraform-provider-registry && go build ./... && go vet ./... && \
		$(GOLANGCI_LINT) run && go test ./internal/...

# Acceptance tests for the Terraform provider against a real registry in
# Docker: apply from nothing, re-plan empty, edit through the API and see the
# drift.
terraform-test:
	./conformance/run-terraform.sh

# Provider reference docs, generated from the schemas and examples/.
TFPLUGINDOCS := $(BIN_DIR)/tfplugindocs

terraform-docs: $(TFPLUGINDOCS)
	cd terraform-provider-registry && $(TFPLUGINDOCS) generate \
		--provider-name registry --rendered-provider-name "Package Registry"

$(TFPLUGINDOCS):
	GOBIN=$(BIN_DIR) go install github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs@latest

# k6 load test ("CI storm") against a warm cache; writes docs/perf.md.
load-test:
	./conformance/run-load.sh

# Conformance against real upstreams (Maven Central, registry.terraform.io).
# Manual run: needs internet access.
conformance-live:
	./conformance/run-live.sh

DEV_COMPOSE := docker compose -f $(COMPOSE_FILE) -f conformance/compose.dev.yml -p registry-dev

dev:
	$(DEV_COMPOSE) up -d --wait minio postgres fake-oidc
	go run ./cmd/registry -config conformance/dev.yaml

# The same stand in the shape it is deployed in: two replicas behind a load
# balancer on the same port, sharing the store and the database. The console
# and every client address http://127.0.0.1:8080 exactly as with `make dev`.
DEV_HA_COMPOSE := docker compose -f $(COMPOSE_FILE) -f conformance/compose.dev-ha.yml -p registry-dev-ha

dev-ha:
	$(DEV_HA_COMPOSE) up -d --build --wait minio postgres fake-oidc registry-1 registry-2 lb

dev-ha-down:
	$(DEV_HA_COMPOSE) down -v

# Live smoke against a stand that is already running: every format, real
# clients, real upstreams. Not hermetic on purpose — conformance proves the
# protocols against fixtures, this proves a deployment.
SMOKE_BASE ?= http://127.0.0.1:8080
SMOKE_LABEL ?= dev
SMOKE_TOKEN ?= $(shell cat .devdata/ci.token 2>/dev/null)

smoke:
	./conformance/smoke/run.sh $(SMOKE_BASE) "$(SMOKE_TOKEN)" $(SMOKE_LABEL)

# Attribution for what the binaries actually link. `notices-check` fails when
# the committed files no longer match the dependency set — a dependency added
# without attribution is a licence violation shipped in a release.
notices:
	python3 scripts/third-party-notices.py

notices-check:
	python3 scripts/third-party-notices.py --check

dev-down:
	$(DEV_COMPOSE) down -v
