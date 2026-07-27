# Runner for the Terraform provider's acceptance tests: a Go toolchain to
# build the provider in-process, and the real terraform CLI to drive it.
#
# The tests never download a provider — terraform-plugin-testing runs the
# provider under test in the same process and hands terraform a reattach
# config — so this stays hermetic once the images are pulled.
FROM hashicorp/terraform:1.9 AS terraform

FROM golang:1.26-alpine
RUN apk add --no-cache git curl
COPY --from=terraform /bin/terraform /usr/local/bin/terraform
ENV TF_ACC=1 \
    CHECKPOINT_DISABLE=1 \
    GOFLAGS=-mod=mod
WORKDIR /src/terraform-provider-registry
