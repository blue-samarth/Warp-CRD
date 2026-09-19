# WebApp API Reference (`webapps.example.com/v1alpha1`)

Types live in [`api/v1alpha1/webapp_types.go`](../api/v1alpha1/webapp_types.go).
The `// +kubebuilder:` marker lines in that file are not documentation — they are
the CRD schema, and `make manifests` regenerates
`config/crd/bases/` from them. Editing the generated YAML by hand is always wrong.

## Design decisions

### Containers are a narrowed projection, not `corev1.Container`

`Container` exposes only the fields a stateless web workload needs: name, image,
ports, env, envFrom, resources, command, args, the three probes, and a narrowed
`securityContext`. Fields that invite foot-guns or break the abstraction
(`hostPort`, `lifecycle`, `volumeMounts`) are deliberately absent. That
narrowing is the product.

`scratchVolumes` is the one volume type exposed, and it exists to make
`readOnlyRootFilesystem` usable: a read-only root with no writable scratch space
breaks any app that writes to `/tmp`, which is most of them. It is `emptyDir`
only — PersistentVolumes are out of scope per the PRD, and ConfigMap/Secret
volumes would reintroduce the mount surface the narrowing exists to avoid.

The generated pod volume is named `<volume>-<hash>`, the hash taken over the
container and volume names together. Pod volume names are pod-wide while these
are declared per container, and the obvious `<container>-<volume>` spelling
fails twice: it is not injective, since `web` + `tmp-cache` and `web-tmp` +
`cache` both yield `web-tmp-cache`, and two 63-character halves overflow the
63-character limit. Hashing closes both without an admission check, which a
`listMapKey` cannot do across containers and a CEL rule could only do by
looping 16 containers × 8 volumes against the cost budget.

`mountPath` rejects `/` and anything under `/proc`, `/sys`, `/dev`, `/etc`,
`/var/run/secrets` and `/run/secrets`. The prefix test appends a separator to
both sides, so `/devices` and `/etcd-data` stay legal while `/dev` and `/etc/ssl`
do not. Shadowing the ServiceAccount token directory does not grant privilege,
but it silently breaks in-cluster API clients, which is worse than refusing it.

`sizeLimit` must be positive — the Quantity pattern admits `-1Gi`, so the
webhook checks the sign — and is required when `medium: Memory`, since a tmpfs
is charged to the container's memory limit and an uncapped one will OOM the pod.

`securityContext` is narrowed rather than omitted. `ContainerSecurityContext`
carries `readOnlyRootFilesystem`, `runAsUser` and `runAsGroup`; it has no
`privileged` and no `capabilities.add`, because the builder always emits the
`restricted`-profile hardening and nothing in the API may unset it. The uid/gid
fields carry `Minimum=1`, so `runAsNonRoot` cannot be contradicted by the same
object that has to satisfy it.

Cost: adding a field later is a code change plus a regenerate, not a free
passthrough. This is the intended trade.

### Conditional rules are CEL rules, with the webhook as a second line

The PRD requires `maxReplicas` when `autoscaling.enabled: true`. OpenAPI cannot
express "required only when a sibling field is true" without a CEL rule. The
objection to CEL is message quality, but `x-kubernetes-validations` carries an
author-written `message`, which makes that cost small next to the benefit: a
schema rule is enforced by the API server, so it survives
`-enable-webhooks=false`, a dead webhook pod, and `failurePolicy: Ignore`.

So `maxReplicas` presence, `maxReplicas >= minReplicas`, and
`strategy.rollingUpdate` only under `RollingUpdate` are all CEL rules on their
enclosing types. The validating webhook keeps equivalent checks — they are what
the unit tests exercise, and they still fire if a CRD is ever installed with the
rules stripped.

CEL rules are charged against a per-rule cost budget derived from the declared
bounds of whatever they traverse. `containers` (`MaxItems=16`) and `ports`
(`MaxItems=32`) carry bounds for that reason: without them the estimator assumes
unbounded lists and the API server refuses to install the CRD.

### The port-name rule matches Kubernetes, not DNS-1123

A DNS-1123 label permits `8080` and `a--b`; `IsValidPortName`, which the Service
and Deployment APIs apply to the objects built from this spec, does not. A
looser rule here admits a WebApp that reconciles into a rejected Service and
sits Degraded forever, so the schema splits the rule in two: a `pattern` for
charset and hyphen placement, and a CEL rule for "at least one letter".

### `status.selector` exists for the scale subresource

Not listed in the PRD's status schema, but required. The `scale` subresource
declares `labelSelectorPath: .status.selector`; without a populated selector,
both `kubectl scale webapp/...` and the HorizontalPodAutoscaler fail to resolve
pods. The controller serializes the managed-pod label selector into it.

### `+listType=map` on containers and ports

Makes server-side apply merge these lists by key (`name` for containers,
`containerPort` for ports) instead of replacing the list wholesale. This matters
as soon as a GitOps tool and a human both apply changes to the same object.

## Port naming and Ingress backend selection

Port names must be unique across the **entire** WebApp, not merely within one
container, because the generated Service flattens all container ports into a
single namespace. The validating webhook enforces this; CEL cannot, since the
rule spans sibling list items.

When `spec.domain` is set, the Ingress backend is chosen as:

1. `spec.ingressPortName`, if set. It must resolve against `resources.PortNames`
   — the same set `backendPort` resolves against, generated `port-<number>`
   names included — or the webhook rejects the WebApp.
2. otherwise the port named `http`, if one exists;
3. otherwise the first declared port.

Rules 2 and 3 are a convention and a fallback guess. `ingressPortName` exists so
a multi-container, multi-port WebApp does not have to rely on either.

## Field defaults

Applied by the OpenAPI schema, and re-applied by the mutating webhook for the
cases the schema cannot express.

| Field | Default |
|---|---|
| `spec.replicas` | `1` (ignored when autoscaling is enabled) |
| `spec.serviceType` | `ClusterIP` |
| `spec.strategy.type` | `RollingUpdate` |
| `spec.containers[].ports[].protocol` | `TCP` |
| `spec.autoscaling.enabled` | `false` |
| `spec.autoscaling.minReplicas` | `1` |
| `spec.autoscaling.targetCPUUtilizationPercentage` | `70`, only when no CPU or memory target is set |

The CPU target is conditional so that a memory-only autoscaler stays
memory-only. Adding a CPU target implicitly would also make `requests.cpu`
mandatory on every container, since the webhook requires a request for every
resource an autoscaling target names.

## Pod security

The builder emits a PodSecurityAdmission `restricted` profile unconditionally:
`runAsNonRoot` and `seccompProfile: RuntimeDefault` on the pod, and
`allowPrivilegeEscalation: false`, `privileged: false`, `runAsNonRoot: true`,
`capabilities.drop: [ALL]` on every container. None of it is configurable.

`spec.securityContext` and `containers[].securityContext` supply only identity
and filesystem settings on top. The consequence is that a root-only image will
not start; that is the intended trade for pods that are admissible in a
`restricted` namespace with no further work.

## Conditions

The controller sets all four on every reconcile, including before the Deployment
exists, so consumers can rely on their presence rather than treating absence as
a state. With no Deployment yet, `Available` and `Progressing` are
`Unknown` with reason `DeploymentNotFound`.

`Ready` and `Available` derive from Deployment status, which derives from pod
readiness. Without a `readinessProbe` a pod is ready as soon as it is running,
so both conditions are only as meaningful as the probe the user supplies.

| Type | Meaning |
|---|---|
| `Progressing` | The Deployment is actively rolling toward the desired spec. |
| `Available` | The Deployment meets its minimum-availability guarantee. |
| `Degraded` | Reconciliation cannot progress: a write failed, or the rollout stalled. |
| `Ready` | Overall roll-up. This is the one developers should watch. |

`status.observedGeneration` records the `metadata.generation` the status was
computed from. A status whose `observedGeneration` lags `metadata.generation`
describes the *previous* spec, not the current one.

## Version policy

`v1alpha1` is currently the storage version (`+kubebuilder:storageversion`).

PRD §6 calls for a later `v1`. No `v1` types and no conversion webhook exist
yet; introducing them means splitting into hub/spoke conversion types, which is
a deliberate follow-up rather than an accident of the current layout.
