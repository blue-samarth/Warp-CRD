# WebApp Operator

A Kubernetes operator that turns a single `WebApp` resource into a managed
Deployment, Service, Ingress and HorizontalPodAutoscaler, and keeps them
reconciled to the declared state.

The point is to let a developer describe an application once, at the level they
actually think about it — containers, ports, a domain, how it scales — instead
of hand-writing four coupled low-level objects and keeping them consistent.

```yaml
apiVersion: webapps.example.com/v1alpha1
kind: WebApp
metadata:
  name: hello
spec:
  containers:
    - name: web
      image: nginxinc/nginx-unprivileged:1.27
      ports:
        - name: http
          containerPort: 8080
```

```
$ kubectl get webapp
NAME    READY   REPLICAS   AVAILABLE   URL   AGE
hello   True    1          1                 30s
```

---

New here? [usage_guide.md](usage_guide.md) is a task-oriented walkthrough.
Working on the code? [docs/internals.md](docs/internals.md) covers every
package and function, and [docs/decisions.md](docs/decisions.md) records what
was fixed, what it cost, and what is still open.

## Contents

- [Quick start](#quick-start)
- [Installation](#installation)
- [The WebApp API](#the-webapp-api)
- [What the operator creates](#what-the-operator-creates)
- [Status and conditions](#status-and-conditions)
- [Events](#events)
- [Metrics](#metrics)
- [Admission: defaulting and validation](#admission-defaulting-and-validation)
- [WebAppPolicy](#webapppolicy)
- [Deletion and finalizers](#deletion-and-finalizers)
- [RBAC](#rbac)
- [Repository layout](#repository-layout)
- [Development](#development)
- [Design decisions](#design-decisions)
- [Troubleshooting](#troubleshooting)
- [Limitations](#limitations)

---

## Quick start

Requires a cluster, `kubectl`, and Go 1.26 for local development.

```
make install                  # install the CRDs only
kubectl apply -f config/samples/webapp_minimal.yaml
kubectl get webapp hello -w
```

To run the controller on your laptop against that cluster, with admission
webhooks disabled (they need serving certs, see below):

```
make run ARGS="-enable-webhooks=false"
```

---

## Installation

### Prerequisites

| | |
|---|---|
| Kubernetes | Built and tested against 1.37 (envtest). The CRD's CEL rules need **1.25+**, and the webhook `matchConditions` guard needs **1.28+** to take effect. Only 1.37 is exercised by the test suite. |
| cert-manager | Required for the admission webhooks. See below for running without it. |
| Go | 1.26 for building and for code generation. |

### With webhooks (recommended)

The webhooks need a TLS serving certificate. `config/default` obtains one from
cert-manager via a self-signed `Issuer`, and wires the CA into both webhook
configurations with the `cert-manager.io/inject-ca-from` annotation.

```
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
make docker-build IMG=<registry>/webapp-operator:v0.1.0
make deploy       IMG=<registry>/webapp-operator:v0.1.0
```

Or render a single file and apply it yourself:

```
make build-installer IMG=<registry>/webapp-operator:v0.1.0
kubectl apply -f dist/install.yaml
```

Everything lands in the `webapp-operator-system` namespace.

### Without cert-manager

Run the manager with `-enable-webhooks=false`. The controller reconciles
normally and the CRD schema keeps enforcing every rule it can express on its
own — `maxReplicas` required under autoscaling, `maxReplicas >= minReplicas`,
port-name and domain formats, `rollingUpdate` only under `RollingUpdate`. See
[Admission](#admission-defaulting-and-validation) for the split.

What you lose is **defaulting** and the rules that need to see the whole object:
duplicate port names across containers, the resource requests an autoscaling
target implies, `ingressPortName` resolution, and `WebAppPolicy`, which is
enforced entirely in the validating webhook.

### Manager flags

| Flag | Default | Purpose |
|---|---|---|
| `-metrics-bind-address` | `:8443` | Metrics endpoint |
| `-metrics-secure` | `true` | Serve metrics over HTTPS with authn/authz |
| `-health-probe-bind-address` | `:8081` | `/healthz` and `/readyz` |
| `-webhook-cert-dir` | `/tmp/k8s-webhook-server/serving-certs` | Where the webhook serving cert is mounted |
| `-metrics-cert-dir` | *(empty)* | Where the metrics serving cert is mounted. Empty serves an unverifiable localhost certificate; the deployment sets it. |
| `-enable-webhooks` | `true` | Serve admission webhooks |
| `-leader-elect` | `false` | Leader election (the deployment sets this) |

---

## The WebApp API

**Group/Version:** `webapps.example.com/v1alpha1` · **Kind:** `WebApp` ·
**Scope:** Namespaced · **Short name:** `wa`

Subresources: `status` and `scale`. `kubectl scale webapp/hello --replicas=3`
works, and so does an external HPA targeting the WebApp.

### spec

| Field | Type | Default | Description |
|---|---|---|---|
| `containers` | `[]Container` | — | **Required**, 1–16. Multiple containers are supported (sidecars). |
| `domain` | `string` | — | When set, an Ingress is created for this host. Must be a valid DNS-1123 subdomain. |
| `ingressClassName` | `string` | — | Ingress class. **Rejected without `domain`.** |
| `ingressPortName` | `string` | — | Names the port the Ingress routes to. **Rejected without `domain`.** See [Ingress backend selection](#ingress-backend-selection). |
| `serviceType` | `ClusterIP\|NodePort\|LoadBalancer` | `ClusterIP` | Type of the generated Service. |
| `replicas` | `int32` | `1` | Desired pods. Defaulted by the schema because the `scale` subresource resolves `.spec.replicas` on every read. **Ignored when autoscaling is enabled**, and applied again when autoscaling is turned off — set it before disabling, or the app returns to this value. |
| `autoscaling` | `AutoscalingSpec` | — | See below. |
| `strategy` | `StrategySpec` | `RollingUpdate` | Rollout behaviour. |
| `imagePullSecrets` | `[]LocalObjectReference` | — | Passed to the pod template. |
| `nodeSelector` | `map[string]string` | — | Passed to the pod template. |
| `tolerations` | `[]Toleration` | — | Passed to the pod template. |
| `affinity` | `Affinity` | — | Passed to the pod template. |
| `serviceAccountName` | `string` | — | Service account for the pods. See [Trust model](#trust-model). |
| `automountServiceAccountToken` | `bool` | `false` | Off by default: the pods are a web workload, and the account is chosen by whoever writes the WebApp. |
| `securityContext` | `PodSecurityContext` | — | `runAsUser`, `runAsGroup`, `fsGroup`, each >= 1. See [Pod security](#pod-security). |
| `podLabels` | `map[string]string` | — | Extra pod-template labels. The operator's own labels win on conflict. |
| `podAnnotations` | `map[string]string` | — | Extra pod-template annotations. |

### spec.containers[]

A deliberately narrowed projection of `corev1.Container`. Fields that are
meaningless or hazardous for a stateless web workload — `hostPort`,
`lifecycle`, `volumeMounts`, and the `securityContext` escapes (`privileged`,
`capabilities.add`) — are not exposed. That narrowing is the product, not an
oversight.

| Field | Type | Default | Description |
|---|---|---|---|
| `name` | `string` | — | **Required.** DNS-1123 label, 1–63 chars, unique within the WebApp. |
| `image` | `string` | — | **Required.** Non-empty. |
| `ports` | `[]ContainerPort` | — | 0–32, see below. At least one port must exist across the whole WebApp. |
| `env` | `[]EnvVar` | — | Standard Kubernetes env vars, including `valueFrom`. |
| `envFrom` | `[]EnvFromSource` | — | `configMapRef` / `secretRef`. |
| `resources` | `ResourceRequirements` | — | Requests and limits. **Requests are required for any resource autoscaling targets.** |
| `command` | `[]string` | — | Overrides the image ENTRYPOINT. |
| `args` | `[]string` | — | Overrides the image CMD. |
| `livenessProbe` | `Probe` | — | Standard Kubernetes probe. |
| `readinessProbe` | `Probe` | — | Standard Kubernetes probe. **`Ready` and `Available` are only as meaningful as this is.** |
| `startupProbe` | `Probe` | — | Standard Kubernetes probe. |
| `securityContext` | `ContainerSecurityContext` | — | `readOnlyRootFilesystem`, `runAsUser`, `runAsGroup`. See [Pod security](#pod-security). |
| `scratchVolumes` | `[]ScratchVolume` | — | 0–8 `emptyDir` volumes. What makes `readOnlyRootFilesystem` usable. See below. |

### spec.containers[].scratchVolumes[]

`emptyDir` only — ephemeral scratch space, not storage. PersistentVolumes are
out of scope, so this is deliberately the whole of it.

| Field | Type | Default | Description |
|---|---|---|---|
| `name` | `string` | — | **Required.** DNS-1123 label, unique within the container. |
| `mountPath` | `string` | — | **Required.** Absolute path, unique within the container. `/` and anything under `/proc`, `/sys`, `/dev`, `/etc`, `/var/run/secrets` or `/run/secrets` is rejected. |
| `medium` | `Disk\|Memory` | `Disk` | `Memory` is a tmpfs, and **counts against the container's memory limit**. |
| `sizeLimit` | `Quantity` | — | Must be positive. **Required when `medium: Memory`.** |

The pod volume is named `<volume>-<hash>`, where the hash is derived from the
container and volume names together. Pod volume names are pod-wide while these
are declared per container, so a plain `<container>-<volume>` concatenation
would not do: it is not injective (container `web` with volume `tmp-cache` and
container `web-tmp` with volume `cache` both give `web-tmp-cache`) and it can
exceed the 63-character limit. Hashing removes both failure modes without an
admission check.

`sizeLimit` is required for `Memory` because a tmpfs with no cap is charged to
the container's memory limit and will OOM the pod. It is optional but strongly
advised for `Disk`, where the ceiling is the node's ephemeral storage.
`WebAppPolicy.spec.maxScratchSize` caps it cluster-side, and treats a volume
with no `sizeLimit` as a violation.

```yaml
containers:
  - name: web
    image: ghcr.io/example/app:1.4.2
    securityContext:
      readOnlyRootFilesystem: true
    scratchVolumes:
      - name: tmp
        mountPath: /tmp
        sizeLimit: 64Mi
```

### spec.containers[].ports[]

| Field | Type | Default | Description |
|---|---|---|---|
| `name` | `string` | — | Optional but recommended. Must be unique across the **entire WebApp**, not just the container, because Service ports share one namespace. Must also be a valid IANA service name: max 15 chars, lowercase alphanumerics and hyphens, **at least one letter**, no consecutive or leading/trailing hyphens. `8080` and `a--b` are rejected. |
| `containerPort` | `int32` | — | **Required.** 1–65535. |
| `protocol` | `TCP\|UDP` | `TCP` | |

The port-name rule is stricter than a DNS-1123 label on purpose: it is exactly
what the Service and Deployment APIs enforce on the objects built from it. A
looser rule here would admit a WebApp that could never reconcile.

### spec.autoscaling

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | `bool` | `false` | While true, `spec.replicas` is ignored and the operator stops managing the Deployment's replica count. |
| `minReplicas` | `int32` | `1` | Lower bound. |
| `maxReplicas` | `int32` | — | **Required when `enabled`.** Must be >= `minReplicas`. Enforced by a CEL rule in the CRD, so it holds without the webhook. |
| `targetCPUUtilizationPercentage` | `int32` | `70`¹ | Average CPU across pods as a percentage of the sum of container CPU requests. **Not capped at 100** — utilization is a ratio of requests, so 150 or 250 are legitimate targets. |
| `targetMemoryUtilizationPercentage` | `int32` | — | Same, for memory. |
| `behavior` | `HorizontalPodAutoscalerBehavior` | — | Passed through to the HPA: `scaleUp` / `scaleDown` stabilization and policies. |

¹ Defaulted to 70 only when **neither** target is set. Setting a memory target
alone gives you a memory-only autoscaler rather than silently adding a CPU one.

Every resource a target names must have a **request on every container** — the
HPA computes utilization against requests, and a missing request leaves it stuck
at `<unknown>` with no signal. Admission rejects that up front rather than
letting it fail silently after the fact.

### spec.strategy

| Field | Type | Default | Description |
|---|---|---|---|
| `type` | `RollingUpdate\|Recreate` | `RollingUpdate` | |
| `rollingUpdate` | `RollingUpdateDeployment` | — | `maxUnavailable` / `maxSurge`. **Only legal when `type: RollingUpdate`** — a CEL rule rejects the combination rather than silently dropping it. |

---

## What the operator creates

| Resource | Created when | Name |
|---|---|---|
| Deployment | always | same as the WebApp |
| Service | always | same as the WebApp |
| Ingress | `spec.domain` is set | same as the WebApp |
| HorizontalPodAutoscaler | `spec.autoscaling.enabled` | same as the WebApp |

All four carry an `OwnerReference` back to the WebApp with `controller: true`,
so the cluster garbage collector removes them once the WebApp is gone. Note the
WebApp itself carries a finalizer, so it is not removed — and GC does not start
— until the operator runs and clears it. See
[Deletion and finalizers](#deletion-and-finalizers).

### Labels

Applied to every managed object:

```
app.kubernetes.io/name:       <webapp name>
app.kubernetes.io/instance:   <webapp name>
app.kubernetes.io/managed-by: webapp-operator
```

The Deployment's **selector** uses all three. `managed-by` is included
deliberately: a selector of only `name` and `instance` overlaps any other tool
(a Helm release, a hand-written Deployment) that happens to set the same two
labels, and selectors are immutable, so this has to be right before `v1alpha1`
ships. `managed-by` is a constant, so including it costs nothing later.

### Service ports

The Service exposes **every** port declared across **all** containers,
de-duplicated by port number **and protocol**, so the same number can appear
once as TCP and once as UDP. A port with no name is given a generated one of
the form `port-<number>` (`port-<number>-udp` for UDP), since a Service with
more than one port requires names.

### Ingress backend selection

When `spec.domain` is set, a single rule is created for that host with path `/`
and `pathType: Prefix`. The backend port is chosen as:

1. `spec.ingressPortName`, if set — it must name a declared port, including a
   generated `port-<number>`, or admission rejects the WebApp;
2. otherwise the port named `http`, if any container declares one;
3. otherwise the first declared port.

Rules 2 and 3 are a convention and a guess respectively. With several containers
and several ports, set `ingressPortName` and stop guessing.

### Pod security

Every generated pod is **PodSecurityAdmission `restricted` compliant** without
the user asking. The operator always sets, and does not let you unset:

```
pod:        runAsNonRoot: true, seccompProfile: RuntimeDefault
container:  allowPrivilegeEscalation: false, privileged: false,
            runAsNonRoot: true, capabilities: {drop: [ALL]}
```

What you can set is identity and filesystem: `spec.securityContext`
(`runAsUser`, `runAsGroup`, `fsGroup`) and `containers[].securityContext`
(`runAsUser`, `runAsGroup`, `readOnlyRootFilesystem`). The uid/gid fields have a
schema minimum of 1, so a root uid is rejected by the API server rather than by
the kubelet at pod start.

### Trust model

**Creating a WebApp is as privileged as creating a Deployment in that
namespace.** `serviceAccountName`, `podLabels` and `podAnnotations` reach the
pod template directly, so whoever can write a WebApp can:

- run pods as **any ServiceAccount in the namespace**, including one more
  privileged than themselves;
- set annotations that other controllers act on — cloud IAM role binding,
  service-mesh sidecar injection, AppArmor profiles;
- set labels that `NetworkPolicy`, `Service` and `PodDisruptionBudget`
  selectors match.

Do not grant WebApp `create`/`update` to anyone you would not grant Deployment
`create`. The schema reserves only the operator's own prefixes
(`app.kubernetes.io/`, `webapps.example.com/`) and caps each map at 32 entries;
it cannot tell a benign annotation from a privilege-granting one, because which
annotations are dangerous depends on what else is installed in the cluster.

For a multi-tenant cluster, constrain these with a `WebAppPolicy` —
`allowedServiceAccounts` and `forbiddenPodAnnotationPrefixes`. See
[WebAppPolicy](#webapppolicy).

The operator's own labels are applied **last** when building the pod template,
so a `podLabels` entry can never break the Deployment's immutable selector even
if the reserved-prefix rule is removed.

The practical consequence of the `restricted` profile: **an image that must run
as root will not start.**
Use a non-root image — `nginxinc/nginx-unprivileged` rather than `nginx`, which
is also why the sample listens on 8080 rather than 80.

### Replicas and the HPA

When autoscaling is enabled, the operator **omits `replicas` entirely** from the
Deployment it applies. Because reconciliation uses server-side apply, omitting
the field means the operator does not own it, so the HPA writes it freely and
the two never fight. With autoscaling off, the operator owns and enforces
`spec.replicas`.

---

## Status and conditions

```yaml
status:
  observedGeneration: 4
  replicas: 3
  readyReplicas: 3
  availableReplicas: 3
  updatedReplicas: 3
  selector: app.kubernetes.io/instance=hello,app.kubernetes.io/name=hello
  ingressURL: http://hello.example.com
  conditions: [...]
```

`observedGeneration` is the `metadata.generation` the status was computed from.
If it lags `metadata.generation`, the status describes the **previous** spec.

`selector` exists for the `scale` subresource — `kubectl scale` and any external
HPA need it to find pods.

All four conditions are set on the first reconcile — before the Deployment
exists, `Available` and `Progressing` are `Unknown/DeploymentNotFound` — so
consumers can read them rather than treat absence as a state.

`Ready` and `Available` are only as trustworthy as your `readinessProbe`.
Without one, a pod counts as ready the moment its container is running, which is
not the same as serving.

| Condition | Meaning |
|---|---|
| `Ready` | Overall roll-up. **This is the one to watch.** |
| `Available` | Mirrors the Deployment's minimum-availability guarantee. |
| `Progressing` | Mirrors the Deployment's rollout progress. |
| `Degraded` | Reconciliation cannot progress, or the rollout has stalled. |

### Condition reasons

| Reason | On | Meaning |
|---|---|---|
| `AllReplicasReady` | Ready=True | Every replica is ready and running the current template. |
| `ReplicasNotReady` | Ready=False | Waiting for replicas. |
| `RolloutStalled` | Ready=False, Degraded=True | Deployment reported `ProgressDeadlineExceeded`. |
| `ReconcileFailed` | Degraded=True | A write failed; the message carries the error. |
| `DeploymentNotFound` | Available/Progressing=Unknown | The Deployment has not reported yet. |
| `MinimumReplicasAvailable` | Available=True | Passed through from the Deployment. |
| `RollingOut` | Progressing=True | Passed through from the Deployment. |
| `Reconciled` | Degraded=False | No degraded state detected. |
| `RolloutPending` | Ready=False | The Deployment has not yet observed the current template. |
| `ScaledToZero` | Ready=False | `replicas: 0`; nothing is expected to run. |

---

## Events

| Reason | Type | Emitted when |
|---|---|---|
| `Reconciled` | Normal | A new spec generation was applied. |
| `Scaled` | Normal | The observed replica count changed (`replicas 2 -> 4`). |
| `Deleted` | Normal | Finalizer cleanup completed. |
| `ReconcileFailed` | Warning | Any reconcile error; the message is the error. |
| `IngressBuildFailed` | Warning | The Ingress could not be built (e.g. no port to route to). |
| `IngressApplyFailed` | Warning | The API server rejected the Ingress. |
| `HPABuildFailed` | Warning | The HPA could not be built — autoscaling is enabled with no `maxReplicas`. |

```
kubectl describe webapp hello
kubectl get events --field-selector involvedObject.kind=WebApp
```

Admission rejections are surfaced by the API server to whoever made the request,
not as Events on an object that was never created.

---

## Metrics

Served on `:8443` over HTTPS, authenticated and authorized against the API
server (`TokenReview` / `SubjectAccessReview`) whenever `-metrics-secure` is
true. A scraper needs the `webapp-operator-metrics-reader` ClusterRole.

The serving certificate comes from cert-manager (`metrics-cert`, issued by the
same self-signed `Issuer` as the webhook) and is mounted at
`-metrics-cert-dir`. Without that flag, controller-runtime serves an in-memory
certificate issued for `localhost`, which nothing can verify by service DNS —
that is the only reason a scrape config would ever need `insecureSkipVerify`,
and `config/prometheus` does not use it. `config/default` installs a
metrics Service plus the `tokenreviews`/`subjectaccessreviews` RBAC that secure
serving requires; `config/prometheus` adds a `ServiceMonitor` for the
prometheus-operator.

| Metric | Type | Labels |
|---|---|---|
| `webapp_reconcile_total` | Counter | `result` (`success`\|`error`) |
| `webapp_reconcile_duration_seconds` | Histogram | `result` |
| `webapp_replicas` | Gauge | `name`, `namespace` |
| `webapp_ready_replicas` | Gauge | `name`, `namespace` |
| `webapp_conditions` | Gauge | `name`, `namespace`, `type`, `status` |

`webapp_conditions` is `1` for the condition's **current** status only. When a
condition flips, the previous series is deleted rather than left behind, so
`webapp_conditions{type="Ready"}` never reports a WebApp as both True and False.

All series for a WebApp are deleted when it is removed.

```
kubectl apply -k config/prometheus     # requires prometheus-operator CRDs
```

Apply it with `-k`, not `-f`: the overlay places the `ServiceMonitor` in the
operator's namespace and rewrites the `serverName` it verifies against. A
`ServiceMonitor` selects Services in its own namespace and reads the CA secret
from there, so a copy applied anywhere else silently scrapes nothing. If you
changed the namespace in `config/default`, change it in
`config/prometheus/kustomization.yaml` too.

---

## Admission: defaulting and validation

### Mutating webhook (defaulting)

| Field | Defaulted to |
|---|---|
| `spec.serviceType` | `ClusterIP` |
| `spec.strategy.type` | `RollingUpdate` |
| `spec.containers[].ports[].protocol` | `TCP` |
| `spec.autoscaling.minReplicas` | `1`, when enabled |
| `spec.autoscaling.targetCPUUtilizationPercentage` | `70`, when enabled and no CPU **or memory** target is set |

`spec.replicas` is absent from this table on purpose: the **schema** defaults
it, not the webhook. Defaulting it in both places made the webhook's branch
unreachable. It cannot be left unset, because the `scale` subresource resolves
`.spec.replicas` on every read and fails with `does not exist` otherwise.

The operator also leaves the WebApp's own labels alone — only the objects it
generates are labelled, so Helm and Argo keep ownership of the WebApp. That
label is what scopes the informer caches: the operator watches only what it
manages, and adoption checks bypass the cache so a foreign object missing from
it is never silently taken over.

Defaulting is idempotent.

### Schema rules (CRD, always on)

These are `pattern`, `minimum`/`maximum` and CEL `x-kubernetes-validations` in
the CRD itself. The API server enforces them before any webhook runs, so they
hold on a webhook-less install — and a rejection carries the message below
rather than a webhook error.

| Rule | Message |
|---|---|
| Container name is a DNS-1123 label | `spec.containers[i].name` pattern |
| Port name is a valid IANA service name | `port name must contain at least one letter (a-z)` / pattern |
| `containerPort` in 1–65535 | `spec.containers[i].ports[j].containerPort` |
| `maxReplicas` set when autoscaling enabled | `spec.autoscaling.maxReplicas is required when autoscaling.enabled is true` |
| `maxReplicas >= minReplicas` | `spec.autoscaling.maxReplicas must be greater than or equal to minReplicas` |
| `rollingUpdate` only with `RollingUpdate` | `may only be set when strategy.type is RollingUpdate` |
| `domain` is a DNS-1123 subdomain | `spec.domain` pattern |
| `ingressClassName` requires `domain` | `spec.ingressClassName is only meaningful with spec.domain` |
| `ingressPortName` requires `domain` | `spec.ingressPortName is only meaningful with spec.domain` |
| uid/gid >= 1 | `spec.securityContext.runAsUser` minimum |
| 1–16 containers, 0–32 ports each | `MinItems` / `MaxItems` |

### Validating webhook

What the schema cannot express — anything needing a cross-field or cross-list
view of the object, or a lookup against other API objects.

| Rule | Message |
|---|---|
| At least one container | `spec.containers: Required value` |
| Container name non-empty and unique | `Required value` / `Duplicate value` |
| Container image non-empty | `spec.containers[i].image: Required value` |
| Port names unique across the whole WebApp | `Duplicate value: "http" (already declared by container "web")` |
| At least one port exists | `at least one container port must be declared` |
| Requests exist for each autoscaling target | `required while autoscaling targets cpu utilization, which is measured against requests` |
| `ingressPortName` names a declared port | `no container declares a port with this name` |
| All `WebAppPolicy` rules | see below |

The webhook also re-checks several schema rules (`maxReplicas`, port-name
format, domain) with a message naming the offending value. The schema rejects
first in a real cluster, so those paths mainly serve the unit tests and any
install whose CRD has been loosened.

Why the port rule exists: a Service with zero ports is rejected by the API
server, so a WebApp with no ports could never reconcile — it would sit Degraded
forever. Better to refuse it at admission with an explanation.

### Warnings (admitted, but flagged)

- `container "web" uses a mutable image tag; pin a version or digest`

---

## WebAppPolicy

Cluster-scoped guardrails, evaluated by the validating webhook. Full detail in
[docs/policy.md](docs/policy.md).

```yaml
apiVersion: webapps.example.com/v1alpha1
kind: WebAppPolicy
metadata:
  name: production-baseline
spec:
  enforcement: Enforce          # or Warn, or Audit
  namespaceSelector:
    matchLabels:
      tier: prod
  maxReplicas: 20
  maxScratchSize: 256Mi
  allowedServiceAccounts: [web, worker]
  forbiddenPodAnnotationPrefixes:
    - iam.amazonaws.com/
    - sidecar.istio.io/
  resources:
    requireRequests: true
    requireLimits: true
    minRequests: {cpu: 50m, memory: 64Mi}
    maxLimits:   {cpu: "4",  memory: 8Gi}
```

- `allowedServiceAccounts`, when non-empty, is the complete set of service
  accounts a WebApp in scope may run as. Unset constrains nothing.
- `forbiddenPodAnnotationPrefixes` rejects pod annotations by prefix. Use it for
  the annotations that grant privilege in *your* cluster — the schema cannot
  know them. See [Trust model](#trust-model).
- `maxScratchSize` caps every `scratchVolumes[].sizeLimit` in scope, and makes
  a volume with no `sizeLimit` a violation. Without it, a `Disk` volume is
  bounded only by the node's ephemeral storage.

- `namespaceSelector` matches the **namespace's** labels. Omitted or empty both
  mean every namespace. **Namespace labels are editable by anyone with namespace
  write access**, so a tenant who can relabel their own namespace can move out
  of a policy's scope. Select on `kubernetes.io/metadata.name` (which the API
  server sets and protects) for policies that must not be escapable, and
  restrict who can patch namespaces.
- Every matching policy is evaluated and violations accumulate; policies do not
  override one another. There is no precedence and no merging — a violation of
  any matching `Enforce` policy rejects the write.
- `maxReplicas` is checked against `spec.autoscaling.maxReplicas` when
  autoscaling is on, since that is the real ceiling, and `spec.replicas` otherwise.
- `minRequests` / `maxLimits` accept only `cpu`, `memory` and
  `ephemeral-storage` — the keys the engine can compare. A policy whose
  `minRequests` exceeds its own `maxLimits` is rejected at policy admission,
  since no container could satisfy both.
- Policy applies only at admission. An already-admitted WebApp keeps running
  until the next write.

### Enforcement modes

| Mode | Write | Feedback |
|---|---|---|
| `Enforce` | rejected | API error to the caller |
| `Warn` | admitted | warning to the caller (`kubectl` prints it) |
| `Audit` | admitted | logged by the operator only |

Roll out with `Audit` to see the blast radius in the operator log without
touching users, move to `Warn` to tell them, then `Enforce`.

### Absent requests and limits are violations

A resource named in `minRequests` or `maxLimits` must be set on every container
in scope. An absent request is zero and an absent limit is unlimited, so
skipping them would let the containers that most need a ceiling slip past it.
Resources the policy does not name are untouched.

`requireRequests` and `requireLimits` remain the weaker assertion that a
container sets requests or limits at all, without naming a bound.

### Scale is enforced by the reconciler

`kubectl scale webapp/x --replicas=N` writes a `Scale`, not a `WebApp`, so the
validating webhook never sees it and `maxReplicas` cannot be enforced at
admission. The reconciler re-checks the ceiling on every pass instead: an
over-scaled WebApp goes `Degraded` with a `PolicyViolation` event and nothing is
applied. `Warn` and `Audit` policies log rather than block, as at admission.

---

## Deletion and finalizers

Finalizer: `webapps.example.com/finalizer`.

On delete the operator removes the owned resources in order — Ingress first so
traffic stops being routed, then HPA, Service and Deployment — drops its metrics
series, emits `Deleted`, and only then removes the finalizer.

**Only objects this WebApp controls are touched.** Every delete reads the object
first and skips it unless its controller `ownerReference` points at this WebApp.
The same check guards the apply path: an existing object owned by something else
is never adopted, and the WebApp goes `Degraded` with
`already exists and is not controlled by this WebApp` instead. Without that,
naming a WebApp after an existing Deployment would delete or overwrite it using
the operator's permissions rather than the requester's.

If any deletion fails, the finalizer is **retained** and the error is returned,
so the WebApp stays visible rather than disappearing with orphans behind it.

Owner references would eventually clean up anyway; the finalizer gives ordered,
observable teardown instead.

---

## RBAC

| Group | Resources | Verbs |
|---|---|---|
| `webapps.example.com` | `webapps`, `webapps/status`, `webapps/finalizers`, `webapppolicies` | full / status / update / read |
| `apps` | `deployments` | full |
| `""` | `services` | full |
| `networking.k8s.io` | `ingresses` | full |
| `autoscaling` | `horizontalpodautoscalers` | full |
| `""` | `namespaces` | get, list, watch (policy namespace selectors) |
| `""` | `events` | create, patch |
| `authentication.k8s.io` / `authorization.k8s.io` | `tokenreviews`, `subjectaccessreviews` | create (secure metrics) |
| `coordination.k8s.io` | `leases` | full, namespaced (leader election) |

`config/rbac/role.yaml` is generated from the `// +kubebuilder:rbac:` markers in
`internal/`. Edit the markers, not the YAML.

---

## Repository layout

```
api/v1alpha1/            WebApp and WebAppPolicy types
                         the // +kubebuilder: markers here ARE the CRD schema
internal/resources/      pure builders: Deployment, Service, Ingress, HPA
internal/controller/     reconciler, status conditions, server-side apply
internal/webhook/        defaulting and validating admission
internal/policy/         WebAppPolicy evaluation
internal/metrics/        Prometheus metrics
cmd/main.go              manager entrypoint
config/                  CRDs, RBAC, webhook, cert-manager, kustomize overlays
test/unit/internal/      table-driven unit tests, mirroring internal/
test/integration/        envtest suites against a real API server
.github/workflows/       CI on every push and PR, release on a v* tag
hack/e2e.sh              end-to-end run against a kind cluster
docs/                    API reference, internals, policy and decisions
usage_guide.md           task-oriented walkthrough
```

---

## Development

Three workflows:

| Workflow | Runs on | Does |
|---|---|---|
| `ci.yml` | every push and pull request | staleness of generated files, gofmt/vet/`go mod tidy -diff`, the unit and envtest suite, every kustomize overlay rendered **and its cross-references asserted**, and the image built |
| `e2e.yml` | **manual only** (`workflow_dispatch`) | `hack/e2e.sh` — a real kind cluster with cert-manager, exercising RBAC, CA injection, the rendered overlays and PodSecurityAdmission |
| `release.yml` | a `v*` tag | re-runs the suite, publishes a multi-arch image to GHCR, attaches `dist/install.yaml` pinned to the published digest |

`e2e.yml` is off the automatic path entirely: it costs about ten minutes, which
is too slow to sit in front of every review. Run it from the Actions tab, or
locally with `make e2e`, **before merging anything that touches reconcile logic,
RBAC or `config/`** — nothing will run it for you.

```
make test              # unit + envtest integration, prints total coverage
make e2e               # real cluster: creates kind, deploys, exercises everything
make verify            # fail if generated files are stale
make build             # bin/manager
make run               # run against the current kubeconfig
make docker-build      # container image
make install           # CRDs into the current cluster
make deploy            # full operator
make build-installer   # dist/install.yaml
make undeploy         # remove the operator, leaving CRDs and WebApps
make uninstall-all     # remove WebApps first, then everything
make clean
```

### Code generation

`api/v1alpha1/zz_generated.deepcopy.go` is **not committed**. It is produced by
`make generate` from the `// +kubebuilder:object:` markers, and nothing compiles
without it — `DeepCopyObject` is what makes the API types satisfy
`runtime.Object`.

A plain `go build ./...` on a fresh clone therefore fails with
`*WebApp does not implement runtime.Object`. Use `make build`, `make test` or
`make run`, which all depend on `generate`. The Dockerfile runs `make generate`
for the same reason. Your editor will show errors across `api/v1alpha1` until
you run it once.

```
make generate      # zz_generated.deepcopy.go
make manifests     # config/crd/bases, config/rbac/role.yaml, config/webhook/manifests.yaml
```

The manifests under `config/` **are** committed, so `kubectl apply` works
without a Go toolchain. Never edit a generated file by hand — `make verify`
regenerates and diffs to catch exactly that.

### Marker lines are not comments

The `// +kubebuilder:` lines in `api/v1alpha1/webapp_types.go` look like
comments but are the input `controller-gen` reads to build the CRD. Deleting
them produces a CRD with no validation, no defaults and no subresources — and
deleting the two package-level markers in `groupversion_info.go` produces a CRD
with an **empty API group**, written to a different filename, which is easy to
miss. Prose documentation belongs in `docs/`; markers belong on the fields.

Note also that a normal doc comment above a field becomes the field's OpenAPI
`description`, which inflates the CRD.

### Tests

Three kinds, all stdlib `testing` — no ginkgo, no testify.

| Location | Package style | Purpose |
|---|---|---|
| `test/unit/internal/<pkg>/` | external `_test` + dot-import | Every unit test, one directory per package under test |
| `test/integration/` | external `_test` | envtest with a real API server and live admission webhooks |
| `hack/e2e.sh` | shell | a real cluster: RBAC, cert-manager CA injection, the rendered overlays, and PodSecurityAdmission enforcing |

All tests live under `test/`; no `_test.go` sits beside the code it covers.
That costs the ability to call unexported functions directly, so the
reconciler is exercised through `Reconcile` rather than through `sync`,
`syncIngress` and `deleteOwned` — a better seam anyway, since it catches
ordering bugs a direct call would step over. The condition helpers in
`internal/controller` are exported for the same reason; `internal/` keeps them
module-private regardless.

Error paths a healthy API server never produces are covered with
`interceptor.Funcs` on the fake client — injecting `Apply` failures, `Delete`
denials and `Update` conflicts.

envtest runs **no Deployment controller and no garbage collector**. Two
consequences the tests rely on: `Deployment.Status` must be written by hand to
exercise condition mapping, and deletion tests genuinely prove the *finalizer*
removed things rather than GC quietly covering for it.

---

## Design decisions

### Server-side apply, not CreateOrUpdate

Reconciliation applies each object with SSA under the field owner
`webapp-operator`. With `CreateOrUpdate`, overwriting `Spec.Template` wholesale
strips the defaults the API server adds (`dnsPolicy`, `terminationMessagePath`,
…); the server re-adds them, the next reconcile strips them again, and the
object is rewritten forever. `TestReconcile_IsIdempotent` asserts the
Deployment's `resourceVersion` does not move across repeated reconciles.

SSA also makes HPA co-ownership fall out for free — see
[Replicas and the HPA](#replicas-and-the-hpa).

Because hand-writing apply configurations for `Affinity`, `EnvVar` and friends
would be thousands of lines, the builders produce ordinary typed objects and
`internal/controller/apply.go` converts them with a generic JSON round-trip. The
builders stay readable and unit-testable; the apply layer stays small.

### Status is patched, not updated

`Status().Update` carries a `resourceVersion`, and the Deployment watch reliably
fires a second reconcile that races the first — producing
`the object has been modified` conflicts. Status is applied with a `MergeFrom`
patch, which carries no `resourceVersion`.

### Conditional rules are CEL first, webhook second

Rules like "`maxReplicas` is required **when** autoscaling is enabled" need a
CEL `x-kubernetes-validations` rule to live in the schema. The argument against
CEL is message quality — but a CEL rule carries a `message` you write yourself,
so that cost is small, and the benefit is large: schema rules are enforced by
the API server, which means they survive `-enable-webhooks=false`, a crashed
webhook pod, and a `failurePolicy: Ignore` someone set in a hurry.

So conditional rules go in the schema wherever CEL can express them, and the
webhook keeps what CEL genuinely cannot see: uniqueness across sibling lists,
cross-object lookups like `WebAppPolicy`, and rules that need the resolved port
set. The webhook's duplicate checks are a second line, not the only one.

CEL has a per-rule cost budget that the estimator computes from the declared
bounds of what a rule walks. That is why `containers` and `ports` carry
`MaxItems` — without them the estimator assumes the worst and the CRD is
rejected at install time.

---

## Troubleshooting

**`WebApp` is Degraded with `ReconcileFailed`.**
`kubectl describe webapp <name>` — the condition message and the
`ReconcileFailed` event carry the underlying API error.

**Everything is rejected with `connect: connection refused` or a TLS error.**
The webhook cannot be reached. Check the operator pod is running, that
cert-manager issued `webhook-server-cert`, and that the `inject-ca-from`
annotation names the namespace the certificate is actually in.

**Nothing is defaulted or validated.**
The webhooks are not installed, or the manager is running with
`-enable-webhooks=false`.

**`kubectl scale` fails with "the spec replicas field cannot be empty".**
`spec.replicas` is unset. It has no schema default — an unset value means the
operator does not manage the field — so set it explicitly before scaling, or
scale a WebApp that already has it.

**Prometheus scrapes return 401.**
Secure metrics serving requires the `tokenreviews`/`subjectaccessreviews`
ClusterRole in `config/rbac/metrics_auth_role.yaml`, and the scraper needs the
`webapp-operator-metrics-reader` role.

**`go build` fails with `does not implement runtime.Object`.**
Run `make generate`. See [Code generation](#code-generation).

**`make verify` fails in CI.**
Someone changed a type or an RBAC marker without regenerating. Run
`make generate manifests` and commit the result.

---

## Limitations

Out of scope by design, per the PRD:

TLS / cert-manager integration for application Ingresses · canary, blue-green
and progressive delivery · service mesh · stateful workloads and
PersistentVolumes · multi-cluster · weighted traffic routing · Job / CronJob ·
NetworkPolicy.

Known gaps in the current implementation:

- **No `v1` API version and no conversion webhook.** `v1alpha1` is the storage
  version. Promoting to `v1` requires hub/spoke conversion types.
- **`ingressURL` is always `http://`.** TLS is out of scope.
- **Ingress is a single host with a single `/` path**, with no annotation
  passthrough. Multiple paths and hosts are not modelled.
- **Only `emptyDir` volumes are exposed**, via `scratchVolumes`. PersistentVolumes,
  ConfigMap and Secret volumes are not modelled; mount configuration through
  `env` / `envFrom` instead.
- **Pods are always `restricted`-profile.** There is no opt-out, so root-only
  images cannot run. See [Pod security](#pod-security).
- **`WebAppPolicy` is admission-only.** It does not retroactively flag WebApps
  admitted before the policy existed, and `Audit` mode writes to the operator
  log rather than to the object.
- **The API group is `webapps.example.com`.** It is baked into the CRD names,
  the finalizer and the leader-election ID. A group cannot be renamed after
  objects are stored, so change it before `v1alpha1` is published anywhere.
