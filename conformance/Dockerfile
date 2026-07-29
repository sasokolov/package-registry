# Multi-stage build of the registry for conformance runs.
# Build context is the repository root: docker build -f conformance/Dockerfile .

# The console is built first and embedded into the binary. Its output is not
# committed, so this stage is the only thing that decides which console a
# given image carries.
FROM node:24-alpine AS console
WORKDIR /ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci
COPY ui/ ./
RUN npm run build

FROM golang:1.26-alpine AS build
ARG GO_BUILD_TAGS=""
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=console /ui/dist ./ui/dist
RUN CGO_ENABLED=0 go build -trimpath -tags "$GO_BUILD_TAGS" -o /out/fondaco ./cmd/fondaco

# alpine, not scratch/distroless: busybox wget is used for container healthchecks.
FROM alpine:3.22
RUN adduser -D -H -u 65532 fondaco && mkdir -p /data && chown fondaco:fondaco /data
COPY --from=build /out/fondaco /usr/local/bin/fondaco
# The image is a distribution channel, and the licence obligations travel with
# what is distributed: whoever pulls it must be able to read the terms and the
# attributions without going to find the repository.
COPY LICENSE NOTICE THIRD-PARTY-NOTICES.md /usr/local/share/fondaco/
USER fondaco
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/fondaco"]
CMD ["-config", "/etc/fondaco/config.yaml"]
