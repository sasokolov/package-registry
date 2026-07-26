# Multi-stage build of the registry for conformance runs.
# Build context is the repository root: docker build -f conformance/Dockerfile .
FROM golang:1.26-alpine AS build
ARG GO_BUILD_TAGS=""
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -tags "$GO_BUILD_TAGS" -o /out/registry ./cmd/registry

# alpine, not scratch/distroless: busybox wget is used for container healthchecks.
FROM alpine:3.22
RUN adduser -D -H -u 65532 registry && mkdir -p /data && chown registry:registry /data
COPY --from=build /out/registry /usr/local/bin/registry
USER registry
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/registry"]
CMD ["-config", "/etc/registry/config.yaml"]
