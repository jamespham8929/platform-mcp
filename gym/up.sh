#!/usr/bin/env bash
# Sets up the incident gym on a kind cluster.
#
#   gym/up.sh          # create the cluster and one shop per scenario
#   gym/up.sh --clean  # delete the cluster
#
# It installs the stack from observability-bootstrap, chaos-operator, and one
# copy of the shop for each file in gym/scenarios. A shop is four copies of
# gitops-platform-demo's sample service:
#
#   loadgen -> frontend -> checkout -> inventory
#   loadgen -> recommendations
#
# Each shop lives in its own namespace, gym-<scenario>, so one scenario's
# fault never shows up in another scenario's recording. The shops need about
# 95 minutes of quiet traffic before `platform-mcp gym record` runs, because
# incident-cli's anomaly detector needs history to compare against.
#
# The other repos are read from sibling folders by default. CI checks them out
# and points these variables at them.
set -euo pipefail

GYM="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPOS="${REPOS:-$(cd "$GYM/../.." && pwd)}"
GITOPS="${GITOPS:-$REPOS/gitops-platform-demo}"
CHAOS="${CHAOS:-$REPOS/chaos-operator}"
OBS="${OBS:-$REPOS/observability-bootstrap}"
CLUSTER="${CLUSTER:-gym}"
SCENARIOS="${SCENARIOS:-$GYM/scenarios}"
STACK_VERSION="${STACK_VERSION:-58.7.2}"
CONTEXT="kind-$CLUSTER"

CURRENT_STEP="setup"
step() {
  CURRENT_STEP="$*"
  printf '\n=== %s ===\n' "$*"
}
on_error() {
  if [[ "${GITHUB_ACTIONS:-}" == "true" ]]; then
    echo "::error title=gym setup failed::failed during \"${CURRENT_STEP}\" (gym/up.sh line $1)"
  fi
}
trap 'on_error "$LINENO"' ERR

if [[ "${1:-}" == "--clean" ]]; then
  kind delete cluster --name "$CLUSTER"
  exit 0
fi

for tool in kind kubectl helm docker; do
  command -v "$tool" > /dev/null || { echo "$tool is required" >&2; exit 1; }
done
for dir in "$GITOPS" "$CHAOS" "$OBS"; do
  [[ -d "$dir" ]] || { echo "missing $dir (set REPOS, GITOPS, CHAOS or OBS)" >&2; exit 1; }
done

k() { kubectl --context "$CONTEXT" "$@"; }

step "Creating kind cluster $CLUSTER"
if ! kind get clusters | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER" --wait 120s
fi

step "Building and loading images"
docker build -q -t sample-service:gym "$GITOPS/apps/sample-service" > /dev/null
docker build -q -t chaos-operator:gym "$CHAOS" > /dev/null
docker build -q -t gym-chaos-agent:gym "$GYM/chaos-agent" > /dev/null
kind load docker-image sample-service:gym chaos-operator:gym gym-chaos-agent:gym --name "$CLUSTER"

step "Installing kube-prometheus-stack $STACK_VERSION"
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts > /dev/null
helm repo update prometheus-community > /dev/null
# The same trimmed install as observability-bootstrap's smoke test. Rules are
# evaluated every 15s so a page fires close to when its condition holds.
helm upgrade --install kube-prometheus-stack prometheus-community/kube-prometheus-stack \
  --kube-context "$CONTEXT" --version "$STACK_VERSION" \
  --namespace monitoring --create-namespace \
  -f "$OBS/helm/prometheus-values.yaml" \
  --set grafana.enabled=false \
  --set alertmanager.alertmanagerSpec.configSecret=null \
  --set alertmanager.alertmanagerSpec.storage=null \
  --set prometheus.prometheusSpec.storageSpec=null \
  --set prometheus.prometheusSpec.retention=12h \
  --set prometheus.prometheusSpec.evaluationInterval=15s \
  --set prometheus.prometheusSpec.resources=null \
  --set prometheusOperator.resources=null \
  --set alertmanager.alertmanagerSpec.resources=null \
  --wait --timeout 15m

step "Installing chaos-operator"
k apply -f "$CHAOS/k8s/crds/chaosexperiment.yaml"
k apply -f "$CHAOS/k8s/operator-deployment.yaml"
k apply -f "$CHAOS/k8s/rbac.yaml"
k -n chaos-system set image deployment/chaos-operator operator=chaos-operator:gym
k -n chaos-system patch deployment chaos-operator --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"}]'
k -n chaos-system rollout status deployment/chaos-operator --timeout=5m

# Every pod gets the chaos-agent sidecar, so any service can be slowed down.
# It runs as root with NET_ADMIN, because tc needs it. Nothing else does.
SIDECAR='[{"name":"chaos-agent","image":"gym-chaos-agent:gym","imagePullPolicy":"Never",
  "securityContext":{"runAsUser":0,"runAsNonRoot":false,"capabilities":{"add":["NET_ADMIN"],"drop":["ALL"]}},
  "resources":{"requests":{"cpu":"5m","memory":"8Mi"}}}]'

deploy_shop() {
  local ns="$1"
  k create namespace "$ns" --dry-run=client -o yaml | k apply -f - > /dev/null
  # Feature flags live here. The gym flips them as distractors.
  k -n "$ns" create configmap flags --dry-run=client -o yaml | k apply -f - > /dev/null

  local common=(
    --kube-context "$CONTEXT" --namespace "$ns"
    --set image.repository=sample-service --set image.tag=gym --set image.pullPolicy=Never
    --set rollout.enabled=false --set autoscaling.enabled=false --set ingress.enabled=false
    --set podDisruptionBudget.enabled=false --set replicaCount=2 --set resources=null
    # A new pod takes 20s to take traffic. A killed pod then leaves a real
    # gap, the way a slow-starting service does.
    --set readinessProbe.initialDelaySeconds=20
    --set-json "extraContainers=$SIDECAR"
  )
  local svc upstream
  for svc in inventory recommendations checkout frontend; do
    case "$svc" in
      checkout) upstream=http://inventory ;;
      frontend) upstream=http://checkout ;;
      *) upstream="" ;;
    esac
    local env='[]'
    [[ -n "$upstream" ]] && env="[{\"name\":\"UPSTREAM_URL\",\"value\":\"$upstream\"}]"
    helm upgrade --install "$svc" "$GITOPS/charts/sample-service" "${common[@]}" \
      --set nameOverride="$svc" --set fullnameOverride="$svc" --set-json "env=$env" > /dev/null
  done

  sed "s/__NAMESPACE__/$ns/g" "$GYM/shop.yaml" | k -n "$ns" apply -f - > /dev/null
}

for file in "$SCENARIOS"/*.json; do
  name="$(basename "$file" .json)"
  step "Deploying the shop for $name"
  deploy_shop "gym-$name"
done

step "Waiting for the shops"
for file in "$SCENARIOS"/*.json; do
  ns="gym-$(basename "$file" .json)"
  for d in inventory recommendations checkout frontend loadgen; do
    k -n "$ns" rollout status "deployment/$d" --timeout=5m > /dev/null
  done
done

cat <<TEXT

The gym is up. Let the shops warm up for about 95 minutes, then record:

  kubectl --context $CONTEXT -n monitoring port-forward svc/kube-prometheus-stack-prometheus 19090:9090 &
  go run ./cmd/platform-mcp gym record --prometheus-url http://127.0.0.1:19090

TEXT
