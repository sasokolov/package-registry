# The fake identity provider, built from the same tree as the registry so it
# cannot drift out of the module.
# Build context is the repository root:
#   docker build -f conformance/fake-oidc/Dockerfile .

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/fake-oidc ./conformance/fake-oidc

FROM alpine:3.22
RUN adduser -D -H idp
COPY --from=build /out/fake-oidc /usr/local/bin/fake-oidc
USER idp
EXPOSE 9099
ENV ADDR=0.0.0.0:9099
ENTRYPOINT ["/usr/local/bin/fake-oidc"]
