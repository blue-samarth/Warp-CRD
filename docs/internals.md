# Internals

Every package, type and function in the operator, and why each one exists.
Generated code (`zz_generated.deepcopy.go`) is omitted — it is mechanical.

For the user-facing API see [api.md](api.md) and
[../README.md](../README.md). For usage see
[../usage_guide.md](../usage_guide.md).

## Contents

- [Package map](#package-map)
- [`api/v1alpha1`](#apiv1alpha1)
- [`internal/resources`](#internalresources)
- [`internal/controller`](#internalcontroller)
- [`internal/policy`](#internalpolicy)
- [`internal/webhook/v1alpha1`](#internalwebhookv1alpha1)
- [`internal/metrics`](#internalmetrics)
- [`cmd`](#cmd)
- [Request lifecycles](#request-lifecycles)

---

## Package map

```
api/v1alpha1/        the CRD schema; markers here ARE the schema
internal/resources/  pure builders: WebApp -> Deployment/Service/Ingress/HPA
internal/controller/ reconcile loop, conditions, server-side apply, finalizer
internal/policy/     WebAppPolicy evaluation, used by webhook and controller
internal/webhook/    defaulting and validating admission
internal/metrics/    Prometheus collectors
cmd/                 manager wiring
```

Dependencies point one way: `resources` imports only `api`; `controller` imports
`resources`, `policy`, `metrics`; `webhook` imports `resources` and `policy`.
Nothing in `resources` takes a `context` or a client, which is what makes it
testable without a cluster.

---

## `api/v1alpha1`

### `groupversion_info.go`

| Symbol | Purpose |
|---|---|
| `GroupVersion` | `webapps.example.com/v1alpha1`. The `// +groupName=` package marker is what puts the group in the generated CRD; delete it and you get a CRD with an empty group written to a different filename. |
| `SchemeBuilder` | Registers the four types with a runtime scheme. |
| `AddToScheme` | Used by `cmd` and every test's scheme. |
| `addKnownTypes` | Registers `WebApp`, `WebAppList`, `WebAppPolicy`, `WebAppPolicyList`. Fails to compile without generated `DeepCopyObject`. |

### `webapp_types.go`

**Constants.**

| Symbol | Value | Notes |
|---|---|---|
| `Finalizer` | `webapps.example.com/finalizer` | Contains the API group, so a group rename touches it. |
| `ConditionReady` `ConditionAvailable` `ConditionProgressing` `ConditionDegraded` | condition type strings | Shared by the controller and every consumer. |

**`Container`** — a narrowed projection of `corev1.Container`.

| Field | Why it is shaped this way |
|---|---|
| `Name` | DNS-1123 label, 1–63, pattern enforced by the schema and re-checked in the webhook. Becomes part of generated volume names. |
| `Image` | Non-empty. No registry or tag policy in the schema; the webhook warns on mutable tags. |
| `Ports` | `MaxItems=32`, `listType=map` keyed on `containerPort`+`protocol` so SSA merges rather than replaces. The bound exists for the CEL cost estimator. |
| `Env`, `EnvFrom` | Passed through whole; the only configuration path, since ConfigMap/Secret volumes are not exposed. |
| `Resources` | Requests become mandatory when autoscaling targets a resource. |
| `Command`, `Args` | ENTRYPOINT / CMD override. |
| `LivenessProbe` `ReadinessProbe` `StartupProbe` | `*corev1.Probe` verbatim. `Ready` is meaningless without the readiness one. |
| `SecurityContext` | `*ContainerSecurityContext`, not the core type — see below. |
| `ScratchVolumes` | `MaxItems=8`, `listType=map` on `name`. The only volume support. |

Deliberately absent: `hostPort`, `lifecycle`, `volumeMounts`, `volumeDevices`,
`privileged`, `capabilities.add`. That narrowing is the product.

**`ScratchMedium`** — `Disk` | `Memory`, enum-constrained at the type.

**`ScratchVolume`**

| Field | Constraints |
|---|---|
| `Name` | DNS-1123 label, unique within the container by `listMapKey`. |
| `MountPath` | Absolute (`^/`), ≤4096. Two CEL rules: one rejects reserved trees (`/`, `/proc`, `/sys`, `/dev`, `/etc`, `/var/run/secrets`, `/run/secrets`), one rejects unclean paths (`//`, `/../`, `/./`, trailing `/.` or `/`). The prefix test appends a separator to both sides so `/devices` and `/etcd-data` stay legal. |
| `Medium` | Defaults `Disk`. |
| `SizeLimit` | `*resource.Quantity`. A type-level CEL rule requires it when `Medium: Memory`, because a tmpfs is charged to the container's memory limit. Must be positive — the Quantity pattern admits `-1Gi`, so the webhook checks the sign. |

**`ContainerSecurityContext`** / **`PodSecurityContext`** — narrowed on purpose.
They carry identity and filesystem settings only (`runAsUser`, `runAsGroup`,
`fsGroup`, `readOnlyRootFilesystem`). The uid/gid fields have `Minimum=1`, so
the schema rejects a root uid rather than leaving the kubelet to fail the pod
against `runAsNonRoot`. Everything that *hardens* the pod is set by the builder
and is not expressible here.

**`ContainerPort`**

The name rule is split in two because neither half alone is right: a `pattern`
handles charset and hyphen placement, and a CEL rule adds "at least one
letter". Together they equal Kubernetes' `IsValidPortName`, which the Service
and Deployment APIs apply downstream. A looser rule would admit a WebApp that
reconciles into a rejected Service.

**`AutoscalingSpec`** — carries two type-level CEL rules: `maxReplicas` required
when `enabled`, and `maxReplicas >= minReplicas`. `TargetCPUUtilizationPercentage`
has **no maximum**: utilisation is a ratio of requests, so 250 is meaningful.
`Behavior` is `*autoscalingv2.HorizontalPodAutoscalerBehavior`, passed through.

**`StrategySpec`** — one CEL rule: `rollingUpdate` only under
`type: RollingUpdate`.

**`WebAppSpec`** — two spec-level CEL rules tie `ingressClassName` and
`ingressPortName` to `domain`. `PodLabels` and `PodAnnotations` are capped at 32
entries and reject the keys the operator owns
(`app.kubernetes.io/{name,instance,managed-by}`, `pod-template-hash`, anything
under `webapps.example.com/`). `Replicas` keeps a schema default of 1 because
the `scale` subresource resolves `.spec.replicas` on every read.
`AutomountServiceAccountToken` defaults to **false**.

**`WebAppStatus`** — `Selector` exists solely for the `scale` subresource's
`labelSelectorPath`; without it `kubectl scale` and any external HPA cannot find
pods. The replica counters mirror Deployment status.

**`WebApp`** — markers here do the heavy lifting: `status` and `scale`
subresources, shortname `wa`, storage version, five print columns, and a
root-level CEL rule on `metadata.name` (DNS-1035, ≤63) because the name becomes
a Service name and a label value.

### `webapppolicy_types.go`

**`EnforcementMode`** — `Enforce` | `Warn` | `Audit`.

**`ResourcePolicy`** — `MinRequests`/`MaxLimits` are `corev1.ResourceList` with
a CEL rule restricting keys to `cpu`, `memory`, `ephemeral-storage`: a key the
engine will not compare is a rule the author believes is in force when it is
not. `MaxProperties` is not usable here — controller-gen rejects it on
`ResourceList`.

**`WebAppPolicySpec`** — `NamespaceSelector` (nil and empty both mean every
namespace), `Resources`, `AllowedServiceAccounts`, `ForbiddenPodAnnotationPrefixes`,
`MaxScratchSize`, `MaxReplicas`, `Enforcement`.

**`WebAppPolicyStatus`** — declared, **never written**. Known gap.

---

## `internal/resources`

Pure functions. No client, no context, no logging.

### `meta.go`

| Symbol | Detail |
|---|---|
| `ErrNoPorts` | Sentinel for "no ports to back an Ingress". |
| `ErrUnknownIngressPort` | `ingressPortName` names nothing. |
| `ManagedByLabel` `NameLabel` `InstanceLabel` `ManagedByValue` | `ManagedByValue` (`webapp-operator`) also scopes the informer caches. |
| `DefaultPortName` | `http` — the backend-selection convention. |
| `SelectorLabels` | `name`, `instance` **and** `managed-by`. All three, so the selector cannot overlap another tool's workload. Selectors are immutable, which is why this had to be right before v1alpha1. |
| `Labels` | Same set; kept separate so the two can diverge later without touching the selector. |
| `portName` | Returns the explicit name, else `port-<n>`, or `port-<n>-udp` for UDP. |
| `protocol` | TCP when unset. |
| `portKey` | `{port, protocol}` — the dedupe key. |
| `allPorts` | Every port across all containers, de-duplicated by number **and** protocol, declaration order preserved. |
| `PortNames` | What `allPorts` will be named, generated names included. Admission checks `ingressPortName` and name collisions against exactly this. |
| `backendPort` | `ingressPortName` → a port named `http` → first declared port. Returns `ErrNoPorts` or `ErrUnknownIngressPort`. |
| `OwnerRefs` | One controller reference with `BlockOwnerDeletion`. Every ownership decision downstream reads this. |

### `deployment.go`

| Symbol | Detail |
|---|---|
| `DesiredReplicas` | `nil` when autoscaling is on, so SSA does not own the field and the HPA writes it freely. Otherwise `spec.replicas`, falling back to 1 when the schema default has not been applied. |
| `Deployment` | Assembles the object. Selector from `SelectorLabels`; template labels from `podLabels`. |
| `strategy` | Drops `rollingUpdate` for `Recreate`, which the Deployment API would reject. |
| `podLabels` | User labels first, operator labels copied **over** them, so a `podLabels` entry can never break the immutable selector even if the schema rule were removed. |
| `automountToken` | Spec value, else `false`. |
| `podSecurityContext` | Always `runAsNonRoot: true` and `seccompProfile: RuntimeDefault`; identity fields from the spec. |
| `containerSecurityContext` | Always `allowPrivilegeEscalation: false`, `privileged: false`, `runAsNonRoot: true`, `capabilities.drop: [ALL]`; `readOnlyRootFilesystem` and uid/gid from the spec. This is what makes the pods `restricted`-admissible. |
| `ScratchVolumeName` | `<volume>-<8 hex of sha256(container\x00volume)>`. Hashed because pod volume names are pod-wide while these are declared per container, and `<container>-<volume>` is not injective: `web`+`tmp-cache` and `web-tmp`+`cache` both give `web-tmp-cache`. Hashing also keeps the result inside 63 characters. |
| `truncate` | Keeps the readable prefix short enough for the hash suffix. |
| `scratchVolumes` | One `emptyDir` per declared volume; `Memory` becomes a tmpfs. |
| `volumeMounts` | Per-container mounts, `nil` when there are none so the field is omitted. |
| `containers` | Projects `v1alpha1.Container` onto `corev1.Container`. The field list here is the real API surface. |
| `containerPorts` | Projection with generated names applied. |

### `service.go`

`Service` builds a ClusterIP-by-default Service selecting on `SelectorLabels`.
`servicePorts` exposes every de-duplicated port with `targetPort` equal to the
container port. A Service with zero ports is rejected by the API server, which
is why the reconciler refuses earlier.

### `ingress.go`

`WantsIngress` is `domain != ""`. `IngressURL` is always `http://` — TLS is out
of scope. `Ingress` builds one rule, one host, path `/`, `pathType: Prefix`,
and returns the `backendPort` error rather than guessing.

### `hpa.go`

| Symbol | Detail |
|---|---|
| `DefaultTargetCPU` | 70. Shared with the defaulter so there is one number. |
| `ErrNoMaxReplicas` | `HPA` refuses to build without `maxReplicas` instead of substituting 1, which would cap the app at one pod while reporting success. |
| `WantsHPA` | `autoscaling.enabled`. |
| `HPA` | `autoscaling/v2` object; `scaleTargetRef` points at the Deployment; `Behavior` passed through. |
| `TargetedResources` | Which resources the HPA will measure. Falls back to CPU when neither target is set, so the object is always valid. Admission requires a request for each of these. |
| `hpaMetrics` | One `MetricSpec` per targeted resource, CPU falling back to `DefaultTargetCPU`. |

---

## `internal/controller`

### `apply.go`

| Symbol | Detail |
|---|---|
| `FieldOwner` | `webapp-operator`. Also the manager name `ReplicasOwnedByOther` compares against. |
| `toApplyConfig[AC]` | Generic JSON round-trip from a typed object to its apply configuration. Hand-writing apply configurations for `Affinity`, `EnvVar` and friends would be thousands of lines; this keeps the builders readable and the apply layer small. Sets the GVK first, which SSA requires. |
| `apply` | `Apply` with `FieldOwner` and `ForceOwnership`. Forcing is safe only because adoption is checked against live state first. |

### `conditions.go`

Ten reason constants: `ReconcileFailed`, `RolloutStalled`, `MinimumReplicasAvailable`,
`RollingOut`, `AllReplicasReady`, `ReplicasNotReady`, `DeploymentNotFound`,
`Reconciled`, `RolloutPending`, `ScaledToZero`.

| Symbol | Detail |
|---|---|
| `SetCondition` | Wraps `meta.SetStatusCondition`, stamping `observedGeneration`. |
| `DeploymentCondition` | Finds a Deployment condition by type, `nil` if absent. |
| `ApplyDeploymentConditions` | Maps Deployment conditions onto `Available` and `Progressing`, detects `ProgressDeadlineExceeded` as stalled, sets `Degraded`, then delegates to `SetReady`. |
| `SetReady` | Ordered: stalled → `RolloutStalled`; `status.observedGeneration < generation` → `RolloutPending`, so a stale cached status cannot claim the new template is up; `desired == 0` → `ScaledToZero`, which used to read `True` via `0 == 0`; all updated and ready → `AllReplicasReady`; else `ReplicasNotReady`. |
| `ApplyNoDeploymentConditions` | All four before the Deployment exists, so consumers never see absence. |
| `ApplyReconcileFailure` | `Degraded`/`Ready` from the error, preserving a previously known `Available`/`Progressing` rather than blanking them. |
| `ReasonOr` | Falls back when the Deployment reports an empty reason. |

### `webapp_controller.go`

**Error classification.** `terminalError` is an error no requeue can clear — it
needs a spec change, which arrives as its own reconcile. `externalError` is
cleared by state outside the object (someone deletes the conflicting Deployment,
or edits the policy) which nothing watches, so it requeues after
`externalRetry` (1 minute). Anything else is returned and gets normal backoff.
`terminal` and `external` are the constructors.

**`WebAppReconciler`** embeds `client.Client` and adds `APIReader`, `Scheme`,
`Recorder`. `reader()` returns `APIReader` when set, else the cached client —
ownership and policy decisions read live state, because the informer caches are
label-scoped and a cached miss would let `ForceOwnership` adopt a foreign
object.

| Method | Detail |
|---|---|
| `Reconcile` | Thin wrapper: times the call and records `webapp_reconcile_total`/`_duration_seconds`. |
| `reconcile` | Get (NotFound → `metrics.Forget`), snapshot `base` for the status patch, branch to `finalize` on deletion, add the finalizer, `sync`, then on success emit `Reconciled` once per generation, set `observedGeneration`, apply conditions and patch status. On failure: conditions, a Warning event, a status patch, then classify the error. |
| `sync` | Builds and checks everything **before** applying anything, so a WebApp that cannot produce a valid set does not leave half of one behind. Order: scale policy, zero-port guard, build Deployment + Service, adoption checks, apply both, then Ingress and HPA. |
| `enforceScalePolicy` | Lists policies and the namespace through `reader()` and runs `policy.EvaluateScale`. This is the only enforcement path for `maxReplicas`, because the `scale` subresource writes a `Scale` object that admission never sees. Violations become an `externalError` plus a `PolicyViolation` event. |
| `replicaHold` | While autoscaling is on and no other manager owns `spec.replicas`, keeps sending the Deployment's current count. Releasing early lets SSA remove a field its sole owner stopped sending, and the Deployment API defaults it back to 1. |
| `ReplicasOwnedByOther` | Parses `managedFields` for a manager other than `FieldOwner` owning `f:spec.f:replicas`. The HPA's *status* is not evidence: it writes the scale only when desired differs from current. |
| `syncIngress` | Deletes the owned Ingress when `domain` is empty; otherwise builds, checks adoption, applies. Build and apply failures get their own Warning events. |
| `syncHPA` | Same shape for the HPA. |
| `deleteOwned` | Reads the object and deletes it **only** if `metav1.IsControlledBy(obj, app)`. Deleting by name would let a WebApp destroy a same-named object using the operator's RBAC rather than the requester's. |
| `assertAdoptable` | Refuses to write an object that exists and is controlled by something else, as an `externalError`. |
| `finalize` | Deletes Ingress → HPA → Service → Deployment. Ingress first so traffic stops being routed before pods go. Emits `Deleted`, drops metrics, then clears the finalizer. A failure retains the finalizer. |
| `updateStatus` | Emits `Scaled` on an observed replica change, copies replica counters, updates the metrics gauges, and patches with `MergeFrom` — which carries no `resourceVersion`, so a reconcile racing the Deployment watch patches instead of conflicting. |
| `SetupWithManager` | `For(&WebApp{})` plus `Owns` on all four kinds. No predicates yet. |

---

## `internal/policy`

**`Result`** — `Violations` (reject), `Warnings` (admit, tell the caller),
`Audited` (admit, log only).

| Symbol | Detail |
|---|---|
| `Matches` | nil selector matches everything; otherwise parses and matches against **namespace** labels. |
| `Evaluate` | Full evaluation. An unparsable selector is **skipped with a warning**, not a violation: denying would fail every WebApp in the cluster, and the policy webhook rejects such selectors at write time. |
| `EvaluateScale` | The replica ceiling alone, for the reconciler's scale path. |
| `record` | Routes an error list into `Violations`, `Warnings` or `Audited` by enforcement mode. The one place the mode is interpreted. |
| `replicaViolations` | Checks `autoscaling.maxReplicas` when autoscaling is on — the real ceiling — and `spec.replicas` otherwise. |
| `violations` | Everything: replicas, service account (an omitted name compares as `default`, since that is what the pod will use), forbidden annotation prefixes, scratch size, then per-container resources. A resource **named** by the policy must be present: an absent request is zero, an absent limit is unlimited. `requireRequests`/`requireLimits` are the weaker "set something" assertion. |
| `Validate` | Policy self-validation for the WebAppPolicy webhook: selector parses, `minRequests <= maxLimits` per resource, no empty strings in `allowedServiceAccounts`, no empty annotation prefix (which would forbid every annotation). |
| `sortedNames` | `slices.Sorted(maps.Keys(l))` — deterministic error ordering. |

---

## `internal/webhook/v1alpha1`

### `webapp_webhook.go`

Carries the `+kubebuilder:webhook:` markers for both WebApp webhooks and the
`+kubebuilder:rbac:` markers for reading policies and namespaces.

| Symbol | Detail |
|---|---|
| `SetupWebAppWebhook` | Wires defaulter and validator. The validator gets `mgr.GetAPIReader()`: a cached read lets a just-created `Enforce` policy be bypassed, and under `failurePolicy: Fail` the first request would block on informer sync. |
| `WebAppDefaulter.Default` | Delegates to `Default`. |
| `WebAppValidator` | Holds a `client.Reader`. |
| `ValidateCreate` | Full validation. |
| `ValidateUpdate` | Returns early when `deletionTimestamp` is set or the spec is unchanged. Without this, tightening an `Enforce` policy blocks the controller's own finalizer removal and strands the object in `Terminating`. The webhook configurations carry a matching `matchConditions` rule so the API server drops those requests even when the webhook is down. |
| `validate` | `ValidateSpec` + `warnings` + policy evaluation; logs `Audited`; wraps errors as `apierrors.NewInvalid`. |
| `evaluatePolicies` | Lists policies, short-circuits when there are none (so the namespace is not fetched), then gets the namespace for its labels. |
| `ValidateDelete` | Always allows. |
| `invalidOrNil` | `nil` or a single `Invalid` carrying the whole list. |
| `warnings` | Mutable image tags only. The tag check looks after the last `/` so `registry:5000/app` is not misread as tagged. |
| `Default` | `serviceType`, `strategy.type`, and under autoscaling `minReplicas` plus a CPU target **only when neither CPU nor memory target is set**. It does not touch `spec.replicas` (the schema does) and does not touch the WebApp's labels — rewriting `managed-by` fought Helm's ownership check and showed as permanent GitOps drift. |
| `ValidateSpec` | What CEL cannot express: at least one container; container name presence, format and uniqueness; image presence; port range; IANA port names; port-name uniqueness across the WebApp; the same port+protocol twice in one pod; scratch mount-path uniqueness and positive `sizeLimit`; at least one port overall; autoscaling bounds and the requests its targets imply; generated-vs-explicit port-name collisions; `ingressPortName` resolution. |
| `missingUtilizationRequests` | One error per container missing a request for each targeted resource. |
| `protocolOf` | TCP when unset, for the port+protocol key. |
| `totalPorts` | Count across containers, for the at-least-one rule. |

### `webapppolicy_webhook.go`

`SetupWebAppPolicyWebhook` and `WebAppPolicyValidator` exist so a policy that
could never be satisfied — `minRequests` above its own `maxLimits`, an empty
annotation prefix, an unparsable selector — is rejected when written rather than
rejecting every WebApp afterwards.

---

## `internal/metrics`

`ResultSuccess`/`ResultError` label values. Five collectors registered with the
controller-runtime registry in `init`: `webapp_reconcile_total`,
`webapp_reconcile_duration_seconds`, `webapp_replicas`,
`webapp_ready_replicas`, `webapp_conditions`.

`SetCondition` deletes the other status series for that condition before
setting the current one, so `webapp_conditions{type="Ready"}` never reports a
WebApp as both `True` and `False`. `Forget` drops every series for a WebApp on
deletion, which is what keeps the cardinality bounded.

---

## `cmd`

`init` registers the client-go and project schemes. `utilruntimeMust` panics on
scheme errors — there is no recovery. `main`:

1. Flags: `-metrics-bind-address`, `-health-probe-bind-address`,
   `-webhook-cert-dir`, `-metrics-cert-dir`, `-leader-elect`,
   `-enable-webhooks`, `-metrics-secure`, plus zap's.
2. Metrics options, adding `filters.WithAuthenticationAndAuthorization` and
   `CertDir` when secure. Without a cert directory controller-runtime serves an
   in-memory certificate for `localhost`, which no scraper can verify by
   service DNS.
3. Cache options scoping Deployment, Service, Ingress and HPA informers to
   `managed-by`. Safe only because adoption reads bypass the cache.
4. Manager with the webhook server and leader-election ID.
5. The reconciler, then both webhooks when enabled.
6. `healthz` is `Ping`; `readyz` is the webhook server's `StartedChecker` when
   webhooks are on, so a rolling update does not route admission traffic to a
   pod that is not listening.

---

## Request lifecycles

### Creating a WebApp

```
kubectl apply
  │
  ├─ schema defaulting            replicas=1, serviceType, protocol, medium…
  ├─ mutating webhook             Default(): strategy, autoscaling targets
  ├─ schema validation (CEL)      bounds, patterns, cross-field rules
  ├─ validating webhook           ValidateSpec + WebAppPolicy (live reads)
  └─ stored
        │
        └─ Reconcile
             ├─ add finalizer
             ├─ enforceScalePolicy            (live policy read)
             ├─ zero-port guard               terminal
             ├─ build Deployment + Service
             ├─ assertAdoptable ×2            external on conflict
             ├─ apply ×2 (SSA, ForceOwnership)
             ├─ syncIngress / syncHPA
             ├─ conditions from Deployment status
             └─ status patch (MergeFrom)
```

### `kubectl scale`

Admission is bypassed entirely — the subresource writes a `Scale`. The write
succeeds, then the next reconcile runs `enforceScalePolicy`, and an
over-scaled WebApp is left `Degraded` with nothing applied.

### Deleting a WebApp

```
kubectl delete
  │
  ├─ deletionTimestamp set; ValidateUpdate skips (and matchConditions
  │  keeps the API server from calling the webhook at all)
  └─ Reconcile → finalize
       ├─ deleteOwned Ingress    (IsControlledBy, else skip)
       ├─ deleteOwned HPA
       ├─ deleteOwned Service
       ├─ deleteOwned Deployment
       ├─ Deleted event, metrics.Forget
       └─ remove finalizer  → API server removes the object
```
