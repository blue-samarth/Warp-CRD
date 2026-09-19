# WebAppPolicy

`WebAppPolicy` is a cluster-scoped resource that constrains what a `WebApp` may
request. It is evaluated by the validating webhook at admission time, so a
violating `WebApp` is rejected before any Deployment is created.

```yaml
apiVersion: webapps.example.com/v1alpha1
kind: WebAppPolicy
metadata:
  name: production-baseline
spec:
  enforcement: Enforce
  namespaceSelector:
    matchLabels:
      tier: prod
  maxReplicas: 20
  resources:
    requireRequests: true
    requireLimits: true
    minRequests:
      cpu: 50m
      memory: 64Mi
    maxLimits:
      cpu: "4"
      memory: 8Gi
```

## Scoping

`namespaceSelector` matches against the **namespace's** labels, not the
WebApp's. An omitted selector and an empty one (`{}`) both match every
namespace — there is no "matches nothing" spelling, and omitting the field is
not a safety default.

Every matching policy is evaluated, and all violations are reported together.
Policies do not override one another; they accumulate. There is deliberately no
precedence or merge rule: a violation of any matching `Enforce` policy rejects
the write, and a stricter policy can never be relaxed by a laxer one that also
matches. The cost is that overlapping policies can report the same problem
twice, each naming its own policy.

## Enforcement modes

| Mode | Write | Feedback |
|---|---|---|
| `Enforce` (default) | rejected | API error to the caller |
| `Warn` | admitted | admission warning to the caller |
| `Audit` | admitted | logged by the operator, nothing to the caller |

The intended rollout is `Audit` → `Warn` → `Enforce`: `Audit` shows you the
blast radius in the operator log without touching anyone, `Warn` tells the
people writing WebApps, `Enforce` stops them.

`Audit` logs to the operator, not to the object, because a validating webhook
cannot annotate the resource it is reviewing. Look for `webapppolicy audit` in
the manager log.

## Replica ceilings

`maxReplicas` applies to whichever field actually controls scale:

- autoscaling disabled, it is checked against `spec.replicas`;
- autoscaling enabled, it is checked against `spec.autoscaling.maxReplicas`,
  since that is the real ceiling.

## Resource rules

`minRequests` and `maxLimits` accept only `cpu`, `memory` and
`ephemeral-storage`. A CEL rule on each map rejects anything else, because a key
the engine will not compare is a rule the author believes is in force when it is
not. Values are compared as quantities, so `250m` and `0.25` compare equal.

A resource **named by the policy** must be present on every container. An
absent request is zero, which is below any floor; an absent limit is unlimited,
which exceeds any ceiling. Treating either as "skip" would make
`maxLimits: {memory: 1Gi}` do nothing for the containers that most need it.
Resources the policy does not name are untouched.

`requireRequests` and `requireLimits` are the separate, weaker assertion that a
container sets *something* — useful when you want requests mandated without
pinning a particular floor.

A policy whose `minRequests` exceeds its own `maxLimits` for the same resource
is rejected when the policy is created: no container could satisfy both, so it
would reject every WebApp in scope. This is what the `WebAppPolicy` validating
webhook exists for.

## Privilege constraints

`serviceAccountName`, `podLabels` and `podAnnotations` pass straight to the pod
template, which makes WebApp `create` as privileged as Deployment `create` in
that namespace. Two policy fields exist to claw that back on a shared cluster:

`allowedServiceAccounts` is an allowlist. Non-empty means a WebApp in scope may
only name one of these; unset constrains nothing. This is the field that stops a
user running pods as a ServiceAccount more privileged than themselves.

`forbiddenPodAnnotationPrefixes` rejects pod annotations by prefix —
`iam.amazonaws.com/` for kube2iam-style role binding, `sidecar.istio.io/` for
mesh injection control, and whatever else confers privilege in your cluster. It
is a denylist by necessity: which annotations are dangerous depends on what
controllers are installed, so the API cannot ship a correct default.

`maxScratchSize` caps `scratchVolumes[].sizeLimit` for every container in
scope, and treats a volume with no `sizeLimit` as a violation rather than
skipping it — an uncapped `Disk` volume is bounded only by the node's ephemeral
storage, so an absent limit is the case worth catching.

The CRD schema independently reserves `app.kubernetes.io/` and
`webapps.example.com/` in `podLabels`, and `webapps.example.com/` in
`podAnnotations`, and caps both maps at 32 entries.

## Interaction with the operator

Most rules are enforced only at admission. A `WebApp` admitted before a policy
was created keeps running; those rules apply the next time that object is
written.

`maxReplicas` is the exception, because it has a second write path the webhook
cannot see. The `scale` subresource sends a `Scale` object, not a `WebApp`, so
`kubectl scale` and an external HPA bypass admission entirely. The reconciler
re-evaluates the ceiling on every pass: an over-scaled WebApp is left
`Degraded` with a `PolicyViolation` event and nothing is applied.

Updates whose spec has not changed, and any update to an object that is being
deleted, skip validation entirely. Without that, tightening an `Enforce` policy
would block the controller from removing its own finalizer and strand the
WebApp in `Terminating`. The webhook configurations carry a matching
`matchConditions` rule, so the API server drops those requests before the
webhook is consulted — deletion still completes when the operator is down and
`failurePolicy: Fail` is in effect.
