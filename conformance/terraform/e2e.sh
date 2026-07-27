#!/bin/sh
# End-to-end proof of the Terraform phase, run inside the acceptance container
# with the real terraform CLI and a real provider binary:
#
#   apply from nothing -> re-plan is empty -> edit through the API is drift
#   -> apply puts it back -> destroy removes it.
#
# The acceptance tests drive the provider in-process; this drives it the way a
# user does, through terraform's own plugin protocol.
set -eu

SRC=/src/terraform-provider-registry
STACK=/tmp/tf-e2e
BIN=/tmp/tf-plugins

echo "==> building the provider binary"
mkdir -p "$BIN" "$STACK"
# -buildvcs=false: the repo is bind-mounted, so git refuses to stamp it
# from inside the container, and a VCS stamp is not what is under test here.
(cd "$SRC" && go build -buildvcs=false -o "$BIN/terraform-provider-registry" .)

# dev_overrides makes terraform use the local binary and skip init entirely,
# so nothing is downloaded and the run stays hermetic.
cat > /tmp/terraformrc <<RC
provider_installation {
  dev_overrides {
    "registry.local/sasokolov/registry" = "$BIN"
  }
  direct {}
}
RC
export TF_CLI_CONFIG_FILE=/tmp/terraformrc
export TF_IN_AUTOMATION=1

cp /src/conformance/terraform/main.tf "$STACK/"
cd "$STACK"

echo "==> apply from nothing"
terraform apply -auto-approve

echo "==> the site reports the feeds it was told to serve"
outputs="$(terraform output -json)"
for want in tf-e2e-central tf-e2e-npm tf-e2e-releases; do
  case "$outputs" in
    *"$want"*) ;;
    *) echo "output does not mention $want: $outputs" >&2; exit 1 ;;
  esac
done

echo "==> the second plan is empty"
set +e
terraform plan -detailed-exitcode -input=false >/tmp/plan.out 2>&1
code=$?
set -e
if [ "$code" -ne 0 ]; then
  echo "a second plan wanted to change something (exit $code):" >&2
  cat /tmp/plan.out >&2
  exit 1
fi

echo "==> the feeds actually serve"
code="$(curl -sS -o /dev/null -w '%{http_code}' \
  "$REGISTRY_ENDPOINT/maven/tf-e2e-central/com/example/liba/1.0.0/liba-1.0.0.jar")"
[ "$code" = "200" ] || { echo "the proxied feed returned $code" >&2; exit 1; }

echo "==> the issued token can publish to the feed that names it"
secret="$(terraform output -raw ci_token)"
[ -n "$secret" ] || { echo "no token secret in state" >&2; exit 1; }
printf 'artifact' > /tmp/a.jar
code="$(curl -sS -o /dev/null -w '%{http_code}' -X PUT \
  -H "Authorization: Bearer $secret" --data-binary @/tmp/a.jar \
  "$REGISTRY_ENDPOINT/maven/tf-e2e-releases/com/example/e2e/1.0.0/e2e-1.0.0.jar")"
[ "$code" = "201" ] || { echo "publishing with the declared token returned $code" >&2; exit 1; }

echo "==> an edit made through the API is drift"
curl -sS -o /dev/null -X PUT \
  -H "Authorization: Bearer $REGISTRY_TOKEN" -H 'Content-Type: application/json' \
  --data '{"name":"tf-e2e-npm","format":"npm","upstream":"http://fake-upstream/npm","anonymous":false}' \
  "$REGISTRY_ENDPOINT/api/v1/config/feeds/tf-e2e-npm"
set +e
terraform plan -detailed-exitcode -input=false >/tmp/plan2.out 2>&1
code=$?
set -e
[ "$code" = "2" ] || {
  echo "the out-of-band edit was not detected as drift (exit $code):" >&2
  cat /tmp/plan2.out >&2
  exit 1
}
grep -q 'tf-e2e-npm' /tmp/plan2.out || { cat /tmp/plan2.out >&2; exit 1; }

echo "==> applying puts it back"
terraform apply -auto-approve
set +e
terraform plan -detailed-exitcode -input=false >/tmp/plan3.out 2>&1
code=$?
set -e
[ "$code" = "0" ] || { echo "still drifting after apply:" >&2; cat /tmp/plan3.out >&2; exit 1; }

echo "==> destroy removes what it created and leaves the packages alone"
terraform destroy -auto-approve
code="$(curl -sS -o /dev/null -w '%{http_code}' \
  "$REGISTRY_ENDPOINT/maven/tf-e2e-central/com/example/liba/1.0.0/liba-1.0.0.jar")"
[ "$code" = "404" ] || { echo "a destroyed feed still serves ($code)" >&2; exit 1; }
# The site itself is untouched: the feeds it was configured with by hand are
# still there.
code="$(curl -sS -o /dev/null -w '%{http_code}' \
  "$REGISTRY_ENDPOINT/maven/central/com/example/liba/1.0.0/liba-1.0.0.jar")"
[ "$code" = "200" ] || { echo "destroy took out a feed it did not own ($code)" >&2; exit 1; }

echo "OK: terraform end-to-end"
