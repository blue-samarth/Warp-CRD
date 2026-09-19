#!/usr/bin/env bash
#
# End-to-end check on a throwaway kind cluster.
#
# Installs what is missing (docker, kubectl, kind, go), creates a cluster when
# no reachable one is configured, deploys the operator with cert-manager, and
# exercises the behaviour the unit and envtest suites cannot: real RBAC, real
# cert-manager CA injection, the rendered kustomize overlays, and
# PodSecurityAdmission actually enforcing.
#
#   ./hack/e2e.sh                 # install, create cluster, test, tear down
#   ./hack/e2e.sh --keep          # leave the cluster running afterwards
#   ./hack/e2e.sh --skip-install  # assume the tooling is already present
#   ./hack/e2e.sh --reuse         # use the current kube context, do not create
#
set -Eeuo pipefail

CLUSTER=${CLUSTER:-webapp-operator-e2e}
IMG=${IMG:-webapp-operator:e2e}
NS=${NS:-webapp-operator-system}
CERT_MANAGER_VERSION=${CERT_MANAGER_VERSION:-v1.16.2}
KIND_VERSION=${KIND_VERSION:-v0.27.0}
TIMEOUT=${TIMEOUT:-180s}

KEEP=false
SKIP_INSTALL=false
REUSE=false
CREATED_CLUSTER=false

while [ $# -gt 0 ]; do
  case "$1" in
    --keep) KEEP=true ;;
    --skip-install) SKIP_INSTALL=true ;;
    --reuse) REUSE=true ;;
    -h|--help) sed -n '3,15p' "$0" | sed 's/^#\{1,\} \{0,1\}//'; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
  shift
done

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$REPO_ROOT"
WORK=$(mktemp -d)

PASS=0
FAIL=0
FAILED_NAMES=()

if [ -t 1 ]; then
  R=$'\033[31m'; G=$'\033[32m'; Y=$'\033[33m'; B=$'\033[1m'; N=$'\033[0m'
else
  R=; G=; Y=; B=; N=
fi

step() { printf '\n%s==> %s%s\n' "$B" "$*" "$N"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '    %s%s%s\n' "$Y" "$*" "$N"; }

# ok/no record a result and keep going, so one broken assertion does not hide
# the rest of the run.
ok() { PASS=$((PASS + 1)); printf '    %s✓%s %s\n' "$G" "$N" "$1"; }
no() {
  FAIL=$((FAIL + 1))
  FAILED_NAMES+=("$1")
  printf '    %s✗%s %s\n' "$R" "$N" "$1"
  [ $# -gt 1 ] && printf '      %s\n' "$2" || true
}

# eq NAME EXPECTED ACTUAL / nonempty NAME VALUE -- plain if/else, because
# "test && ok || no" runs both arms when ok itself fails.
eq() {
  if [ "$2" = "$3" ]; then ok "$1"; else no "$1" "expected '$2', got '${3:-empty}'"; fi
}
nonempty() {
  if [ -n "$2" ]; then ok "$1"; else no "$1" "empty"; fi
}

# check NAME COMMAND...  -- passes when the command succeeds
check() {
  local name=$1; shift
  local out
  if out=$("$@" 2>&1); then ok "$name"; else no "$name" "${out//$'\n'/ | }"; fi
}

# rejects NAME FILE -- passes when the API server refuses the manifest, and the
# message contains the expected substring
rejects() {
  local name=$1 file=$2 want=$3 out
  if out=$(kubectl apply -f "$file" 2>&1); then
    no "$name" "accepted, but should have been rejected"
    return
  fi
  if printf '%s' "$out" | grep -qi -- "$want"; then
    ok "$name"
  else
    no "$name" "rejected for the wrong reason: ${out//$'\n'/ | }"
  fi
}

# make deploy stamps the image into this file via `kustomize edit set image`,
# which would otherwise leave the working tree dirty after a local run.
IMAGE_STAMP=config/manager/kustomization.yaml
STAMP_BACKUP=

cleanup() {
  local rc=$?
  set +e
  if [ -n "$STAMP_BACKUP" ] && [ -f "$STAMP_BACKUP" ]; then
    cp "$STAMP_BACKUP" "$IMAGE_STAMP"
  fi
  if [ "$CREATED_CLUSTER" = true ] && [ "$KEEP" = false ]; then
    step "Tearing down"
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
    info "deleted kind cluster $CLUSTER"
  elif [ "$CREATED_CLUSTER" = true ]; then
    warn "cluster $CLUSTER left running (--keep); remove with: kind delete cluster --name $CLUSTER"
  fi
  rm -rf "$WORK"
  exit $rc
}
trap cleanup EXIT

# ---------------------------------------------------------------- tooling

need_root() { [ "$(id -u)" -eq 0 ] && SUDO="" || SUDO="sudo"; }

install_tooling() {
  step "Checking tooling"
  need_root

  if ! grep -qi ubuntu /etc/os-release 2>/dev/null; then
    warn "not Ubuntu; skipping package installation and assuming the tooling is present"
    SKIP_INSTALL=true
  fi

  if [ "$SKIP_INSTALL" = false ]; then
    $SUDO apt-get update -qq
    $SUDO apt-get install -y -qq curl ca-certificates git make jq >/dev/null
  fi

  if ! command -v docker >/dev/null; then
    if [ "$SKIP_INSTALL" = true ]; then
      echo "docker is required and not installed" >&2; exit 1
    fi
    info "installing docker"
    curl -fsSL https://get.docker.com | $SUDO sh >/dev/null
    $SUDO usermod -aG docker "$USER" || true
  fi
  if ! docker info >/dev/null 2>&1; then
    echo "docker is installed but not usable by $USER. Log out and back in (group change), or run with sudo." >&2
    exit 1
  fi
  info "docker $(docker version --format '{{.Server.Version}}' 2>/dev/null || echo present)"

  if ! command -v kubectl >/dev/null; then
    info "installing kubectl"
    local v
    v=$(curl -fsSL https://dl.k8s.io/release/stable.txt)
    curl -fsSLo "$WORK/kubectl" "https://dl.k8s.io/release/${v}/bin/linux/amd64/kubectl"
    $SUDO install -m 0755 "$WORK/kubectl" /usr/local/bin/kubectl
  fi
  info "kubectl $(kubectl version --client -o json | jq -r .clientVersion.gitVersion)"

  if ! command -v kind >/dev/null; then
    info "installing kind $KIND_VERSION"
    curl -fsSLo "$WORK/kind" "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-amd64"
    $SUDO install -m 0755 "$WORK/kind" /usr/local/bin/kind
  fi
  info "kind $(kind version | awk '{print $2}')"

  if ! command -v go >/dev/null; then
    echo "go is required to build the operator image (go.mod needs 1.26). Install it and re-run." >&2
    exit 1
  fi
  info "go $(go version | awk '{print $3}')"
}

# ---------------------------------------------------------------- cluster

ensure_cluster() {
  step "Cluster"
  if kubectl cluster-info >/dev/null 2>&1; then
    info "using the current context: $(kubectl config current-context)"
    if [ "$REUSE" = false ] && ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
      warn "this is not the $CLUSTER kind cluster; the run will create and delete objects in it"
      warn "pass --reuse to acknowledge, or unset KUBECONFIG to get a throwaway cluster"
      [ "$REUSE" = false ] && { echo "refusing to touch an unrecognised cluster" >&2; exit 1; }
    fi
    return
  fi

  info "no reachable cluster configured; creating kind cluster $CLUSTER"
  kind create cluster --name "$CLUSTER" --wait 120s
  CREATED_CLUSTER=true
  kubectl cluster-info >/dev/null
  info "server $(kubectl version -o json | jq -r .serverVersion.gitVersion)"
}

install_cert_manager() {
  step "cert-manager $CERT_MANAGER_VERSION"
  if kubectl get deploy -n cert-manager cert-manager-webhook >/dev/null 2>&1; then
    info "already installed"
  else
    kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml" >/dev/null
  fi
  kubectl -n cert-manager wait --for=condition=Available --timeout="$TIMEOUT" \
    deploy/cert-manager deploy/cert-manager-webhook deploy/cert-manager-cainjector >/dev/null
  info "ready"
}

deploy_operator() {
  step "Building and deploying the operator"
  STAMP_BACKUP="$WORK/kustomization.yaml.orig"
  cp "$IMAGE_STAMP" "$STAMP_BACKUP"
  make docker-build IMG="$IMG" >/dev/null
  info "built $IMG"

  if [ "$CREATED_CLUSTER" = true ] || kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    kind load docker-image "$IMG" --name "$CLUSTER" >/dev/null
    info "loaded into kind"
  else
    warn "not a kind cluster; $IMG must already be pullable from the nodes"
  fi

  make deploy IMG="$IMG" >/dev/null
  kubectl -n "$NS" rollout status deploy/webapp-operator-controller-manager --timeout="$TIMEOUT" >/dev/null
  info "operator rolled out"

  # The webhook is useless until cert-manager has injected the CA, and every
  # WebApp write fails closed until then.
  local tries=0
  until [ -n "$(kubectl get validatingwebhookconfiguration \
        webapp-operator-validating-webhook-configuration \
        -o jsonpath='{.webhooks[0].clientConfig.caBundle}' 2>/dev/null)" ]; do
    tries=$((tries + 1))
    [ "$tries" -gt 60 ] && { echo "CA was never injected into the webhook configuration" >&2; exit 1; }
    sleep 2
  done
  info "CA injected"
}

ns() { # create a fresh namespace and echo its name
  local n="e2e-$1"
  kubectl create namespace "$n" >/dev/null 2>&1 || true
  echo "$n"
}

wait_ready() { kubectl -n "$1" wait --for=condition=Ready --timeout="$TIMEOUT" "webapp/$2" >/dev/null 2>&1; }
wait_degraded() { kubectl -n "$1" wait --for=condition=Degraded --timeout=60s "webapp/$2" >/dev/null 2>&1; }

# ---------------------------------------------------------------- tests

test_deployment_stack() {
  step "A WebApp becomes a Deployment and a Service"
  local n; n=$(ns basic)
  cat >"$WORK/basic.yaml" <<EOF
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata: {name: hello, namespace: $n}
spec:
  containers:
    - name: web
      image: nginxinc/nginx-unprivileged:1.27
      ports: [{name: http, containerPort: 8080}]
EOF
  check "minimal WebApp is admitted" kubectl apply -f "$WORK/basic.yaml"
  check "Deployment created" kubectl -n "$n" get deploy hello
  check "Service created" kubectl -n "$n" get svc hello
  check "no Ingress without a domain" bash -c "! kubectl -n $n get ingress hello >/dev/null 2>&1"
  check "no HPA without autoscaling" bash -c "! kubectl -n $n get hpa hello >/dev/null 2>&1"

  if wait_ready "$n" hello; then ok "Ready=True"; else
    no "Ready=True" "$(kubectl -n "$n" get webapp hello -o jsonpath='{.status.conditions}')"
  fi

  local sel
  sel=$(kubectl -n "$n" get deploy hello -o jsonpath='{.spec.selector.matchLabels}')
  case "$sel" in
    *managed-by*) ok "selector includes managed-by" ;;
    *) no "selector includes managed-by" "$sel" ;;
  esac

  local url
  url=$(kubectl -n "$n" get webapp hello -o jsonpath='{.status.selector}')
  nonempty "status.selector populated for the scale subresource" "$url"
}

test_restricted_psa() {
  step "Pods satisfy PodSecurityAdmission restricted"
  local n; n=$(ns psa)
  # If the operator's hardening is wrong, the ReplicaSet cannot create pods here
  # and the failure is explicit rather than theoretical.
  kubectl label namespace "$n" \
    pod-security.kubernetes.io/enforce=restricted \
    pod-security.kubernetes.io/enforce-version=latest --overwrite >/dev/null

  cat >"$WORK/psa.yaml" <<EOF
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata: {name: hardened, namespace: $n}
spec:
  containers:
    - name: web
      image: nginxinc/nginx-unprivileged:1.27
      ports: [{name: http, containerPort: 8080}]
EOF
  kubectl apply -f "$WORK/psa.yaml" >/dev/null

  if kubectl -n "$n" wait --for=jsonpath='{.status.availableReplicas}'=1 \
      --timeout="$TIMEOUT" deploy/hardened >/dev/null 2>&1; then
    ok "pods admitted and available under enforce=restricted"
  else
    no "pods admitted under enforce=restricted" \
       "$(kubectl -n "$n" get events --field-selector reason=FailedCreate -o jsonpath='{.items[-1:].message}')"
  fi

  local sc
  sc=$(kubectl -n "$n" get deploy hardened -o json |
       jq -r '.spec.template.spec.containers[0].securityContext | "\(.allowPrivilegeEscalation) \(.runAsNonRoot) \(.capabilities.drop[0])"')
  eq "container hardening (allowPrivilegeEscalation/runAsNonRoot/drop)" "false true ALL" "$sc"

  local amt
  amt=$(kubectl -n "$n" get deploy hardened -o jsonpath='{.spec.template.spec.automountServiceAccountToken}')
  eq "service account token not mounted by default" "false" "$amt"
}

test_ingress() {
  step "Ingress and backend port selection"
  local n; n=$(ns ingress)
  cat >"$WORK/ing.yaml" <<EOF
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata: {name: routed, namespace: $n}
spec:
  domain: routed.example.com
  ingressPortName: admin
  containers:
    - name: web
      image: nginxinc/nginx-unprivileged:1.27
      ports:
        - {name: http, containerPort: 8080}
        - {name: admin, containerPort: 9901}
EOF
  check "WebApp with a domain is admitted" kubectl apply -f "$WORK/ing.yaml"
  check "Ingress created" kubectl -n "$n" get ingress routed

  local port host url
  port=$(kubectl -n "$n" get ingress routed -o jsonpath='{.spec.rules[0].http.paths[0].backend.service.port.name}')
  eq "ingressPortName wins over the http convention" "admin" "$port"

  host=$(kubectl -n "$n" get ingress routed -o jsonpath='{.spec.rules[0].host}')
  eq "ingress host set" "routed.example.com" "$host"

  url=$(kubectl -n "$n" get webapp routed -o jsonpath='{.status.ingressURL}')
  eq "status.ingressURL reported" "http://routed.example.com" "$url"

  # Clearing the domain must remove the Ingress again.
  kubectl -n "$n" patch webapp routed --type=json \
    -p '[{"op":"remove","path":"/spec/domain"},{"op":"remove","path":"/spec/ingressPortName"}]' >/dev/null
  local tries=0
  until ! kubectl -n "$n" get ingress routed >/dev/null 2>&1; do
    tries=$((tries + 1)); [ "$tries" -gt 30 ] && break; sleep 2
  done
  check "Ingress removed when the domain is cleared" \
    bash -c "! kubectl -n $n get ingress routed >/dev/null 2>&1"
}

test_scaling() {
  step "Scaling through spec and through the scale subresource"
  local n; n=$(ns scale)
  cat >"$WORK/scale.yaml" <<EOF
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata: {name: scaled, namespace: $n}
spec:
  replicas: 2
  containers:
    - name: web
      image: nginxinc/nginx-unprivileged:1.27
      ports: [{name: http, containerPort: 8080}]
EOF
  kubectl apply -f "$WORK/scale.yaml" >/dev/null
  check "spec.replicas reaches the Deployment" \
    kubectl -n "$n" wait --for=jsonpath='{.spec.replicas}'=2 --timeout="$TIMEOUT" deploy/scaled

  check "kubectl scale works (scale subresource)" kubectl -n "$n" scale webapp/scaled --replicas=3
  check "scaled count propagates" \
    kubectl -n "$n" wait --for=jsonpath='{.spec.replicas}'=3 --timeout="$TIMEOUT" deploy/scaled
}

test_autoscaling() {
  step "Autoscaling"
  local n; n=$(ns hpa)

  cat >"$WORK/hpa-bad.yaml" <<EOF
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata: {name: no-requests, namespace: $n}
spec:
  autoscaling: {enabled: true, maxReplicas: 5}
  containers:
    - name: web
      image: nginxinc/nginx-unprivileged:1.27
      ports: [{name: http, containerPort: 8080}]
EOF
  rejects "autoscaling without CPU requests is rejected" "$WORK/hpa-bad.yaml" "resources.requests"

  cat >"$WORK/hpa-nomax.yaml" <<EOF
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata: {name: no-max, namespace: $n}
spec:
  autoscaling: {enabled: true}
  containers:
    - name: web
      image: nginxinc/nginx-unprivileged:1.27
      ports: [{name: http, containerPort: 8080}]
      resources: {requests: {cpu: 50m}}
EOF
  rejects "autoscaling without maxReplicas is rejected by CEL" "$WORK/hpa-nomax.yaml" "maxReplicas is required"

  cat >"$WORK/hpa-ok.yaml" <<EOF
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata: {name: auto, namespace: $n}
spec:
  autoscaling:
    enabled: true
    minReplicas: 2
    maxReplicas: 6
    targetCPUUtilizationPercentage: 250
  containers:
    - name: web
      image: nginxinc/nginx-unprivileged:1.27
      ports: [{name: http, containerPort: 8080}]
      resources: {requests: {cpu: 50m}}
EOF
  check "utilisation target above 100 is accepted" kubectl apply -f "$WORK/hpa-ok.yaml"
  check "HPA created" kubectl -n "$n" get hpa auto
  local bounds
  bounds=$(kubectl -n "$n" get hpa auto -o jsonpath='{.spec.minReplicas}/{.spec.maxReplicas}')
  eq "HPA bounds match the spec" "2/6" "$bounds"

  # Turning autoscaling off again must remove the HPA.
  kubectl -n "$n" patch webapp auto --type=merge \
    -p '{"spec":{"autoscaling":{"enabled":false},"replicas":2}}' >/dev/null
  local tries=0
  until ! kubectl -n "$n" get hpa auto >/dev/null 2>&1; do
    tries=$((tries + 1)); [ "$tries" -gt 30 ] && break; sleep 2
  done
  check "HPA removed when autoscaling is disabled" \
    bash -c "! kubectl -n $n get hpa auto >/dev/null 2>&1"
}

test_scratch_and_probes() {
  step "Probes, scratch volumes and a read-only root"
  local n; n=$(ns scratch)
  cat >"$WORK/scratch.yaml" <<EOF
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata: {name: scratchy, namespace: $n}
spec:
  containers:
    - name: web
      image: nginxinc/nginx-unprivileged:1.27
      ports: [{name: http, containerPort: 8080}]
      readinessProbe: {httpGet: {path: /, port: http}, initialDelaySeconds: 2}
      livenessProbe: {httpGet: {path: /, port: http}, periodSeconds: 10}
      scratchVolumes:
        - {name: tmp, mountPath: /tmp, sizeLimit: 64Mi}
        - {name: cache, mountPath: /var/cache, medium: Memory, sizeLimit: 32Mi}
EOF
  check "probes and scratch volumes are admitted" kubectl apply -f "$WORK/scratch.yaml"

  local vols mounts medium
  vols=$(kubectl -n "$n" get deploy scratchy -o json | jq '[.spec.template.spec.volumes[] | select(.emptyDir)] | length')
  eq "two emptyDir volumes generated" "2" "$vols"

  mounts=$(kubectl -n "$n" get deploy scratchy -o json | jq -r '[.spec.template.spec.containers[0].volumeMounts[].mountPath] | sort | join(",")')
  eq "mount paths as declared" "/tmp,/var/cache" "$mounts"

  medium=$(kubectl -n "$n" get deploy scratchy -o json | jq -r '[.spec.template.spec.volumes[] | select(.emptyDir.medium=="Memory")] | length')
  eq "Memory medium becomes a tmpfs" "1" "$medium"

  check "readinessProbe reaches the container" \
    bash -c "kubectl -n $n get deploy scratchy -o jsonpath='{.spec.template.spec.containers[0].readinessProbe.httpGet.path}' | grep -q /"

  cat >"$WORK/scratch-bad.yaml" <<EOF
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata: {name: bad-mount, namespace: $n}
spec:
  containers:
    - name: web
      image: nginxinc/nginx-unprivileged:1.27
      ports: [{name: http, containerPort: 8080}]
      scratchVolumes: [{name: bad, mountPath: /etc/ssl}]
EOF
  rejects "reserved mount path is rejected" "$WORK/scratch-bad.yaml" "reserved mount path"

  cat >"$WORK/scratch-unclean.yaml" <<EOF
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata: {name: unclean, namespace: $n}
spec:
  containers:
    - name: web
      image: nginxinc/nginx-unprivileged:1.27
      ports: [{name: http, containerPort: 8080}]
      scratchVolumes: [{name: bad, mountPath: /tmp/../etc}]
EOF
  rejects "path traversal is rejected" "$WORK/scratch-unclean.yaml" "clean absolute path"

  cat >"$WORK/scratch-tmpfs.yaml" <<EOF
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata: {name: unbounded, namespace: $n}
spec:
  containers:
    - name: web
      image: nginxinc/nginx-unprivileged:1.27
      ports: [{name: http, containerPort: 8080}]
      scratchVolumes: [{name: tmp, mountPath: /tmp, medium: Memory}]
EOF
  rejects "Memory medium without sizeLimit is rejected" "$WORK/scratch-tmpfs.yaml" "sizeLimit is required"
}

test_validation() {
  step "Schema and webhook validation"
  local n; n=$(ns validate)

  _bad() { # name, want, spec-body
    cat >"$WORK/v.yaml" <<EOF
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata: {name: $1, namespace: $n}
spec:
$3
EOF
    rejects "$1" "$WORK/v.yaml" "$2"
  }

  _bad digit-port "at least one letter" "$(cat <<'EOF'
  containers:
    - name: web
      image: nginx:1.27
      ports: [{name: "8080", containerPort: 8080}]
EOF
)"

  _bad ip-domain "IP addresses" "$(cat <<'EOF'
  domain: 10.0.0.1
  containers:
    - name: web
      image: nginx:1.27
      ports: [{name: http, containerPort: 8080}]
EOF
)"

  _bad orphan-class "only meaningful with spec.domain" "$(cat <<'EOF'
  ingressClassName: nginx
  containers:
    - name: web
      image: nginx:1.27
      ports: [{name: http, containerPort: 8080}]
EOF
)"

  _bad recreate-rolling "may only be set when strategy.type is RollingUpdate" "$(cat <<'EOF'
  strategy: {type: Recreate, rollingUpdate: {maxSurge: 1}}
  containers:
    - name: web
      image: nginx:1.27
      ports: [{name: http, containerPort: 8080}]
EOF
)"

  _bad reserved-label "are reserved" "$(cat <<'EOF'
  podLabels: {app.kubernetes.io/name: hijacked}
  containers:
    - name: web
      image: nginx:1.27
      ports: [{name: http, containerPort: 8080}]
EOF
)"

  _bad dup-port "share one network namespace" "$(cat <<'EOF'
  containers:
    - name: web
      image: nginx:1.27
      ports: [{name: http, containerPort: 8080}]
    - name: side
      image: nginx:1.27
      ports: [{name: admin, containerPort: 8080}]
EOF
)"

  _bad no-ports "at least one container port" "$(cat <<'EOF'
  containers:
    - name: web
      image: nginx:1.27
EOF
)"

  _bad root-uid "runAsUser" "$(cat <<'EOF'
  securityContext: {runAsUser: 0}
  containers:
    - name: web
      image: nginx:1.27
      ports: [{name: http, containerPort: 8080}]
EOF
)"

  cat >"$WORK/badname.yaml" <<EOF
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata: {name: 1app, namespace: $n}
spec:
  containers:
    - name: web
      image: nginx:1.27
      ports: [{name: http, containerPort: 8080}]
EOF
  rejects "a name that is not a DNS-1035 label is rejected" "$WORK/badname.yaml" "must start with a letter"

  # Defaulting: the schema supplies replicas, the webhook supplies the rest, and
  # neither touches the user's own labels.
  cat >"$WORK/defaults.yaml" <<EOF
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata:
  name: defaulted
  namespace: $n
  labels: {app.kubernetes.io/managed-by: Helm}
spec:
  containers:
    - name: web
      image: nginxinc/nginx-unprivileged:1.27
      ports: [{name: http, containerPort: 8080}]
EOF
  kubectl apply -f "$WORK/defaults.yaml" >/dev/null
  local got
  got=$(kubectl -n "$n" get webapp defaulted \
    -o jsonpath='{.spec.replicas}/{.spec.serviceType}/{.spec.strategy.type}/{.spec.containers[0].ports[0].protocol}')
  eq "defaults applied (replicas/serviceType/strategy/protocol)" "1/ClusterIP/RollingUpdate/TCP" "$got"

  got=$(kubectl -n "$n" get webapp defaulted -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}')
  eq "the WebApp's own labels are left alone" "Helm" "$got"
}

test_ownership() {
  step "Ownership: no adoption, no collateral deletion"
  local n; n=$(ns ownership)

  # A Deployment this WebApp does not control must not be taken over.
  kubectl -n "$n" create deployment squatter --image=nginxinc/nginx-unprivileged:1.27 >/dev/null
  cat >"$WORK/adopt.yaml" <<EOF
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata: {name: squatter, namespace: $n}
spec:
  containers:
    - name: web
      image: nginxinc/nginx-unprivileged:1.27
      ports: [{name: http, containerPort: 8080}]
EOF
  kubectl apply -f "$WORK/adopt.yaml" >/dev/null
  if wait_degraded "$n" squatter; then
    local msg
    msg=$(kubectl -n "$n" get webapp squatter -o jsonpath='{.status.conditions[?(@.type=="Degraded")].message}')
    case "$msg" in
      *"not controlled by this WebApp"*) ok "adoption of a foreign Deployment is refused" ;;
      *) no "adoption refused" "Degraded says: $msg" ;;
    esac
  else
    no "adoption refused" "the WebApp never went Degraded"
  fi
  check "the foreign Deployment still has its own owner" \
    bash -c "test -z \"\$(kubectl -n $n get deploy squatter -o jsonpath='{.metadata.ownerReferences}')\""

  # An Ingress the operator does not own must survive the delete-when-no-domain
  # path that runs on every reconcile.
  local m; m=$(ns ownership-keep)
  cat >"$WORK/keep.yaml" <<EOF
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata: {name: keeper, namespace: $m}
spec:
  containers:
    - name: web
      image: nginxinc/nginx-unprivileged:1.27
      ports: [{name: http, containerPort: 8080}]
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: keeper, namespace: $m}
spec:
  rules:
    - host: someone-else.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend: {service: {name: other, port: {number: 80}}}
EOF
  kubectl apply -f "$WORK/keep.yaml" >/dev/null
  wait_ready "$m" keeper || true
  sleep 5
  check "a foreign same-named Ingress is not deleted" kubectl -n "$m" get ingress keeper
}

test_policy() {
  step "WebAppPolicy"
  local n; n=$(ns policy)
  kubectl label namespace "$n" tier=prod --overwrite >/dev/null

  cat >"$WORK/badpolicy.yaml" <<'EOF'
apiVersion: webapps.example.com/v1alpha1
kind: WebAppPolicy
metadata: {name: e2e-impossible}
spec:
  enforcement: Enforce
  resources:
    minRequests: {cpu: "2"}
    maxLimits: {cpu: "1"}
EOF
  rejects "a policy with minRequests above maxLimits is rejected" "$WORK/badpolicy.yaml" "must not exceed maxLimits"

  cat >"$WORK/emptyprefix.yaml" <<'EOF'
apiVersion: webapps.example.com/v1alpha1
kind: WebAppPolicy
metadata: {name: e2e-emptyprefix}
spec:
  enforcement: Enforce
  forbiddenPodAnnotationPrefixes: [""]
EOF
  rejects "an empty annotation prefix is rejected" "$WORK/emptyprefix.yaml" "forbids every annotation"

  cat >"$WORK/policy.yaml" <<'EOF'
apiVersion: webapps.example.com/v1alpha1
kind: WebAppPolicy
metadata: {name: e2e-prod}
spec:
  enforcement: Enforce
  namespaceSelector: {matchLabels: {tier: prod}}
  maxReplicas: 3
  allowedServiceAccounts: [default]
  forbiddenPodAnnotationPrefixes: [iam.amazonaws.com/]
  resources:
    minRequests: {cpu: 50m}
EOF
  check "policy is created" kubectl apply -f "$WORK/policy.yaml"

  cat >"$WORK/toomany.yaml" <<EOF
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata: {name: too-many, namespace: $n}
spec:
  replicas: 9
  containers:
    - name: web
      image: nginxinc/nginx-unprivileged:1.27
      ports: [{name: http, containerPort: 8080}]
      resources: {requests: {cpu: 50m}}
EOF
  rejects "replica ceiling is enforced at admission" "$WORK/toomany.yaml" "at most 3 replicas"

  cat >"$WORK/nocpu.yaml" <<EOF
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata: {name: no-cpu, namespace: $n}
spec:
  containers:
    - name: web
      image: nginxinc/nginx-unprivileged:1.27
      ports: [{name: http, containerPort: 8080}]
EOF
  rejects "an absent request violates minRequests" "$WORK/nocpu.yaml" "absent request is zero"

  cat >"$WORK/iam.yaml" <<EOF
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata: {name: iam-annot, namespace: $n}
spec:
  podAnnotations: {iam.amazonaws.com/role: arn:aws:iam::1:role/admin}
  containers:
    - name: web
      image: nginxinc/nginx-unprivileged:1.27
      ports: [{name: http, containerPort: 8080}]
      resources: {requests: {cpu: 50m}}
EOF
  rejects "a forbidden annotation prefix is rejected" "$WORK/iam.yaml" "forbids the prefix"

  cat >"$WORK/compliant.yaml" <<EOF
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata: {name: compliant, namespace: $n}
spec:
  replicas: 2
  containers:
    - name: web
      image: nginxinc/nginx-unprivileged:1.27
      ports: [{name: http, containerPort: 8080}]
      resources: {requests: {cpu: 50m}}
EOF
  check "a compliant WebApp is admitted" kubectl apply -f "$WORK/compliant.yaml"

  # The scale subresource writes a Scale, which admission never sees, so the
  # ceiling has to be caught by the reconciler instead.
  kubectl -n "$n" scale webapp/compliant --replicas=9 >/dev/null
  if wait_degraded "$n" compliant; then
    local msg
    msg=$(kubectl -n "$n" get webapp compliant -o jsonpath='{.status.conditions[?(@.type=="Degraded")].message}')
    case "$msg" in
      *"at most 3 replicas"*) ok "kubectl scale cannot bypass the ceiling" ;;
      *) no "scale bypass caught" "Degraded says: $msg" ;;
    esac
  else
    no "scale bypass caught" "the WebApp never went Degraded after kubectl scale"
  fi
  local actual
  actual=$(kubectl -n "$n" get deploy compliant -o jsonpath='{.spec.replicas}')
  if [ "${actual:-0}" -le 3 ]; then
    ok "the Deployment never exceeded the ceiling (${actual})"
  else
    no "ceiling held" "Deployment has $actual replicas"
  fi

  # A namespace outside the selector is untouched.
  local o; o=$(ns policy-other)
  cat >"$WORK/other.yaml" <<EOF
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata: {name: unscoped, namespace: $o}
spec:
  replicas: 9
  containers:
    - name: web
      image: nginxinc/nginx-unprivileged:1.27
      ports: [{name: http, containerPort: 8080}]
EOF
  check "an unlabelled namespace is out of scope" kubectl apply -f "$WORK/other.yaml"

  kubectl delete webapppolicy e2e-prod >/dev/null 2>&1 || true
}

test_deletion() {
  step "Deletion and finalizer"
  local n; n=$(ns deletion)
  cat >"$WORK/del.yaml" <<EOF
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata: {name: doomed, namespace: $n}
spec:
  domain: doomed.example.com
  autoscaling: {enabled: true, maxReplicas: 3}
  containers:
    - name: web
      image: nginxinc/nginx-unprivileged:1.27
      ports: [{name: http, containerPort: 8080}]
      resources: {requests: {cpu: 50m}}
EOF
  kubectl apply -f "$WORK/del.yaml" >/dev/null
  for k in deploy svc ingress hpa; do
    kubectl -n "$n" get "$k" doomed >/dev/null 2>&1 || sleep 3
  done
  check "all four objects exist before deletion" \
    bash -c "kubectl -n $n get deploy,svc,ingress,hpa doomed >/dev/null 2>&1"

  local fin
  fin=$(kubectl -n "$n" get webapp doomed -o jsonpath='{.metadata.finalizers[0]}')
  eq "finalizer set" "webapps.example.com/finalizer" "$fin"

  check "delete returns" kubectl -n "$n" delete webapp doomed --timeout=90s
  check "WebApp is gone (finalizer cleared)" bash -c "! kubectl -n $n get webapp doomed >/dev/null 2>&1"
  for k in deploy svc ingress hpa; do
    check "owned $k removed" bash -c "! kubectl -n $n get $k doomed >/dev/null 2>&1"
  done
}

test_deployment_shape() {
  step "Operator deployment shape"
  local want
  want=$(kubectl -n "$NS" get deploy webapp-operator-controller-manager -o jsonpath='{.spec.replicas}')
  eq "two replicas for webhook availability" "2" "$want"

  check "PodDisruptionBudget present" kubectl -n "$NS" get pdb webapp-operator-controller-manager
  check "metrics certificate issued" kubectl -n "$NS" get secret metrics-server-cert
  check "webhook certificate issued" kubectl -n "$NS" get secret webhook-server-cert
  check "cluster-scoped names are prefixed" kubectl get clusterrole webapp-operator-manager-role

  local mem
  mem=$(kubectl -n "$NS" get deploy webapp-operator-controller-manager \
    -o jsonpath='{.spec.template.spec.containers[0].resources.limits.memory}')
  eq "memory limit raised to 512Mi" "512Mi" "$mem"

  local ready
  ready=$(kubectl -n "$NS" get deploy webapp-operator-controller-manager -o json |
    jq -r '.spec.template.spec.containers[0].readinessProbe.httpGet.path')
  eq "readyz probe wired" "/readyz" "$ready"
}

test_metrics_auth() {
  step "Metrics require authentication"
  local n; n=$(ns metrics)
  # Unauthenticated scrape of the HTTPS endpoint must not return metrics.
  local out
  out=$(kubectl -n "$n" run curl-probe --rm -i --restart=Never --quiet \
    --image=curlimages/curl:8.11.1 --command -- \
    curl -sk -o /dev/null -w '%{http_code}' \
    "https://webapp-operator-controller-manager-metrics-service.$NS.svc:8443/metrics" 2>/dev/null || true)
  case "$out" in
    401|403) ok "unauthenticated scrape rejected with $out" ;;
    "") warn "could not run the probe pod (image pull?); skipping" ;;
    *) no "unauthenticated scrape rejected" "got HTTP $out" ;;
  esac
}

# Dumped rather than pointed at, because the cluster is gone by the time anyone
# reads a CI log.
dump_diagnostics() {
  step "Operator logs (last 200 lines)"
  kubectl -n "$NS" logs deploy/webapp-operator-controller-manager \
    --all-containers --tail=200 2>&1 | sed 's/^/    /' || true

  step "Operator pods"
  kubectl -n "$NS" get pods -o wide 2>&1 | sed 's/^/    /' || true

  step "Recent warnings across the test namespaces"
  kubectl get events -A --field-selector type=Warning \
    --sort-by=.lastTimestamp 2>&1 | tail -40 | sed 's/^/    /' || true

  step "WebApps still present"
  kubectl get webapps -A 2>&1 | sed 's/^/    /' || true
}

# ---------------------------------------------------------------- run

main() {
  install_tooling
  ensure_cluster
  install_cert_manager
  deploy_operator

  test_deployment_shape
  test_deployment_stack
  test_restricted_psa
  test_ingress
  test_scaling
  test_autoscaling
  test_scratch_and_probes
  test_validation
  test_ownership
  test_policy
  test_deletion
  test_metrics_auth

  step "Summary"
  printf '    %s%d passed%s' "$G" "$PASS" "$N"
  if [ "$FAIL" -gt 0 ]; then
    printf ', %s%d failed%s\n' "$R" "$FAIL" "$N"
    for f in "${FAILED_NAMES[@]}"; do printf '      %s- %s%s\n' "$R" "$f" "$N"; done
    dump_diagnostics
    return 1
  fi
  printf '\n'
  return 0
}

main
