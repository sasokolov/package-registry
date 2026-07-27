#!/usr/bin/env bash
# Installs the chart into a throwaway kind cluster and smoke-tests it:
# the pods become ready, /healthz answers, a feed proxies a package from a
# fake upstream inside the cluster, and a rolling restart keeps serving.
#
#   deploy/helm/smoke.sh            create the cluster, test, delete it
#   KEEP_CLUSTER=1 deploy/helm/smoke.sh   leave the cluster for debugging
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
CLUSTER="${CLUSTER:-registry-smoke}"
NAMESPACE=registry
RELEASE=smoke
IMAGE="package-registry:smoke"
KEEP_CLUSTER="${KEEP_CLUSTER:-0}"

for tool in kind kubectl helm docker; do
  command -v "$tool" >/dev/null || { echo "$tool is required" >&2; exit 1; }
done

cleanup() {
  if [[ "$KEEP_CLUSTER" == "1" ]]; then
    echo "==> KEEP_CLUSTER=1: cluster $CLUSTER left running"
    return
  fi
  kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> building the registry image"
docker build -q -f "$REPO_ROOT/conformance/Dockerfile" -t "$IMAGE" "$REPO_ROOT" >/dev/null

echo "==> the chart renders a config the registry itself accepts"
# A chart that renders an invalid config only fails at rollout, when the
# pods crash-loop. Check every shape the chart can produce, up front.
extract_config() { # <rendered helm output> <destination>
  python3 -c '
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
cm = next(d for d in docs if d["kind"] == "ConfigMap" and "config.yaml" in d.get("data", {}))
open(sys.argv[2], "w").write(cm["data"]["config.yaml"])
' "$1" "$2"
}

render_and_check() { # <description> <helm args...>
  local desc="$1"; shift
  local rendered
  rendered="$(mktemp -d)"
  chmod 755 "$rendered"
  helm template check "$SCRIPT_DIR/registry" "$@" > "$rendered/all.yaml"
  extract_config "$rendered/all.yaml" "$rendered/config.yaml"
  chmod 644 "$rendered/config.yaml"
  # The mTLS material comes from a Secret at runtime; the check only needs
  # the paths to exist, since what it verifies is the config's shape.
  mkdir -p "$rendered/replication"
  : > "$rendered/replication/ca.crt"
  : > "$rendered/replication/tls.crt"
  : > "$rendered/replication/tls.key"
  : > "$rendered/replication/token"
  chmod -R a+rX "$rendered/replication"
  # Every ${VAR} the config references must be injected by the Deployment.
  if ! docker run --rm -v "$rendered:/cfg:ro" \
    -v "$rendered/replication:/etc/registry/replication:ro" \
    -e REGISTRY_S3_ACCESS_KEY=access -e REGISTRY_S3_SECRET_KEY=secret \
    -e REGISTRY_DATABASE_DSN=postgres://u:p@h/db \
    "$IMAGE" config check -config /cfg/config.yaml; then
    echo "chart config is invalid ($desc):" >&2
    cat "$rendered/config.yaml" >&2
    rm -rf "$rendered"
    exit 1
  fi
  rm -rf "$rendered"
  echo "    ok: $desc"
}

render_and_check "defaults"
render_and_check "filesystem storage, no database" \
  --set storage.type=fs --set database.existingSecret=""
render_and_check "geo replication" \
  --set replication.enabled=true \
  --set 'replication.peerCIDRs={10.0.0.0/8}' \
  --set-json 'replication.peers=[{"name":"us-1","url":"https://us.example.com:8443","public_url":"https://us.example.com","pull_interval":"10s"}]'
render_and_check "extra config sections" \
  --set-json 'extraConfig={"server":{"projection_repair":"10m"},"auth":{"stale_identity_window":"0s"}}'

echo "==> creating kind cluster $CLUSTER"
kind create cluster --name "$CLUSTER" --wait 120s >/dev/null
kind load docker-image "$IMAGE" --name "$CLUSTER" >/dev/null
kubectl create namespace "$NAMESPACE" >/dev/null

echo "==> deploying a fake upstream inside the cluster"
kubectl -n "$NAMESPACE" create configmap fake-upstream-content \
  --from-literal=artifact.jar="hello from the smoke upstream" >/dev/null
# Serve the fixture under a real Maven coordinate; nginx maps the path onto
# the single ConfigMap file (ConfigMap keys cannot contain slashes).
kubectl -n "$NAMESPACE" create configmap fake-upstream-conf --from-literal=default.conf='
server {
    listen 80;
    location = /com/example/lib/1.0.0/lib-1.0.0.jar {
        default_type application/java-archive;
        alias /usr/share/nginx/html/data/artifact.jar;
    }
}
' >/dev/null
kubectl -n "$NAMESPACE" apply -f - >/dev/null <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: fake-upstream
spec:
  replicas: 1
  selector: {matchLabels: {app: fake-upstream}}
  template:
    metadata: {labels: {app: fake-upstream}}
    spec:
      containers:
        - name: nginx
          image: nginx:1.28-alpine
          ports: [{containerPort: 80}]
          volumeMounts:
            - name: content
              mountPath: /usr/share/nginx/html/data
            - name: conf
              mountPath: /etc/nginx/conf.d
      volumes:
        - name: content
          configMap: {name: fake-upstream-content}
        - name: conf
          configMap: {name: fake-upstream-conf}
---
apiVersion: v1
kind: Service
metadata:
  name: fake-upstream
spec:
  selector: {app: fake-upstream}
  ports: [{port: 80, targetPort: 80}]
YAML
kubectl -n "$NAMESPACE" rollout status deploy/fake-upstream --timeout=120s >/dev/null

echo "==> installing the chart (filesystem storage, no database)"
helm install "$RELEASE" "$SCRIPT_DIR/registry" \
  --namespace "$NAMESPACE" \
  --set image.repository=package-registry \
  --set image.tag=smoke \
  --set image.pullPolicy=Never \
  --set storage.type=fs \
  --set storage.fs.path=/tmp/registry-data \
  --set database.existingSecret="" \
  --set autoscaling.enabled=false \
  --set replicaCount=2 \
  --set-json 'feeds=[{"name":"smoke","format":"maven","upstream":"http://fake-upstream","anonymous":true}]' \
  --wait --timeout 180s >/dev/null

echo "==> pods are ready"
kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/instance="$RELEASE" --no-headers

svc="svc/$RELEASE-registry"
run_curl() { # <path> [curl args...]
  local path="$1"; shift
  kubectl -n "$NAMESPACE" run "smoke-curl-$RANDOM" --rm -i --restart=Never --quiet \
    --image=curlimages/curl:8.12.1 --command -- \
    curl -sS "$@" "http://$RELEASE-registry$path"
}

echo "==> health endpoints answer"
[[ "$(run_curl /healthz)" == "ok" ]] || { echo "healthz failed" >&2; exit 1; }
run_curl /metrics | grep -q registry_site_info || { echo "metrics missing" >&2; exit 1; }

echo "==> the feed proxies a package from the fake upstream"
body="$(run_curl /maven/smoke/com/example/lib/1.0.0/lib-1.0.0.jar)"
if [[ "$body" != "hello from the smoke upstream" ]]; then
  echo "proxy returned: $body" >&2
  exit 1
fi
# The cache check must target one replica: this smoke install uses
# filesystem storage without a shared volume, so each pod has its own cache
# (S3 is the supported multi-replica backend — see deploy/helm/README.md).
pod="$(kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/instance="$RELEASE" \
  -o jsonpath='{.items[0].metadata.name}')"
in_pod() { kubectl -n "$NAMESPACE" exec "$pod" -- "$@"; }
in_pod wget -q -O /dev/null http://127.0.0.1:8080/maven/smoke/com/example/lib/1.0.0/lib-1.0.0.jar
headers="$(in_pod wget -S -q -O /dev/null \
  http://127.0.0.1:8080/maven/smoke/com/example/lib/1.0.0/lib-1.0.0.jar 2>&1)"
if ! grep -qi 'X-Registry-Source: cache' <<<"$headers"; then
  echo "second fetch on the same replica was not served from cache:" >&2
  echo "$headers" >&2
  exit 1
fi

echo "==> rolling restart keeps the service answering"
kubectl -n "$NAMESPACE" rollout restart deploy/"$RELEASE"-registry >/dev/null
kubectl -n "$NAMESPACE" rollout status deploy/"$RELEASE"-registry --timeout=180s >/dev/null
[[ "$(run_curl /healthz)" == "ok" ]] || { echo "healthz failed after restart" >&2; exit 1; }

echo "==> PDB and service exist"
kubectl -n "$NAMESPACE" get pdb "$RELEASE-registry" >/dev/null
kubectl -n "$NAMESPACE" get "$svc" >/dev/null

echo "OK: helm smoke passed"
