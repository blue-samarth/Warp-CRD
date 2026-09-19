# WebApp Operator — Usage Guide

A task-oriented walkthrough. For the field-by-field API surface see
[README.md](README.md); for how the code works see
[docs/internals.md](docs/internals.md).

## Contents

- [Install](#install)
- [Your first WebApp](#your-first-webapp)
- [Reading status](#reading-status)
- [Exposing it on a domain](#exposing-it-on-a-domain)
- [Scaling](#scaling)
- [Autoscaling](#autoscaling)
- [Configuration and secrets](#configuration-and-secrets)
- [Probes](#probes)
- [Writable scratch space](#writable-scratch-space)
- [Sidecars](#sidecars)
- [Guardrails with WebAppPolicy](#guardrails-with-webapppolicy)
- [Deleting things](#deleting-things)
- [Common errors](#common-errors)
- [What this operator will not do](#what-this-operator-will-not-do)

---

## Install

### CRDs only, for a quick look

```
make install
kubectl apply -f config/samples/webapp_minimal.yaml
```

Nothing reconciles yet — there is no operator running. Use this to explore the
schema with `kubectl explain webapp.spec`.

### Run the operator from your laptop

```
make run ARGS="-enable-webhooks=false"
```

Webhooks need a serving certificate, which a local process does not have. The
CRD's own rules still apply; what you lose is defaulting and the checks that
need to see the whole object. See
[Without cert-manager](README.md#without-cert-manager).

### Full install into a cluster

```
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
make docker-build IMG=<registry>/webapp-operator:v0.1.0
make deploy       IMG=<registry>/webapp-operator:v0.1.0
```

cert-manager is required: it issues the webhook and metrics certificates and
injects the CA into both webhook configurations. Everything lands in
`webapp-operator-system`.

Verify:

```
kubectl -n webapp-operator-system get deploy,pod,svc
kubectl get validatingwebhookconfiguration | grep webapp
```

---

## Your first WebApp

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
kubectl apply -f hello.yaml
kubectl get webapp hello -w
```

You get a Deployment, a Service, and nothing else — no Ingress without
`domain`, no HPA without `autoscaling`.

**The image must run as non-root.** Every generated pod is built for the
PodSecurityAdmission `restricted` profile, and that is not optional. `nginx`
will not start; `nginxinc/nginx-unprivileged` will. This is the single most
common first surprise.

```
kubectl get deploy,svc -l app.kubernetes.io/instance=hello
```

---

## Reading status

```
$ kubectl get webapp
NAME    READY   REPLICAS   AVAILABLE   URL   AGE
hello   True    1          1                 30s
```

`READY` is the one to watch. When it is not `True`:

```
kubectl describe webapp hello
```

Four conditions are always present:

| Condition | Read it for |
|---|---|
| `Ready` | the roll-up — is this app serving the current spec |
| `Available` | the Deployment's minimum-availability guarantee |
| `Progressing` | rollout in flight |
| `Degraded` | something is wrong, with the reason in the message |

`Ready` is only as honest as your `readinessProbe`. Without one, a pod counts
as ready the moment its container is running — which is not the same as
serving. See [Probes](#probes).

`observedGeneration` tells you whether status describes the spec you just
applied. If it lags `metadata.generation`, the operator has not caught up, or
it failed — check `Degraded`.

---

## Exposing it on a domain

```yaml
spec:
  domain: hello.example.com
  ingressClassName: nginx
  containers:
    - name: web
      image: ghcr.io/example/app:1.4.2
      ports:
        - name: http
          containerPort: 8080
```

One Ingress, one host, path `/` with `pathType: Prefix`. `status.ingressURL`
becomes `http://hello.example.com` — always `http`, because TLS is out of
scope.

**Which port does it route to?** In order: `spec.ingressPortName` if set, then
a port named `http`, then the first declared port. With more than one port, set
it explicitly:

```yaml
spec:
  domain: hello.example.com
  ingressPortName: http
```

`ingressClassName` and `ingressPortName` are both rejected without `domain` —
they would be silently ignored otherwise.

---

## Scaling

```yaml
spec:
  replicas: 4
```

or

```
kubectl scale webapp/hello --replicas=4
```

Both work; the `scale` subresource is why `spec.replicas` always has a value.
An external HPA can target the WebApp directly for the same reason.

If a `WebAppPolicy` caps replicas, `kubectl scale` does **not** fail — the
subresource writes a `Scale` object, which admission never sees. The reconciler
refuses instead, so watch for `Degraded` with a `PolicyViolation` event.

---

## Autoscaling

```yaml
spec:
  autoscaling:
    enabled: true
    minReplicas: 2
    maxReplicas: 10
    targetCPUUtilizationPercentage: 70
  containers:
    - name: web
      image: ghcr.io/example/app:1.4.2
      ports:
        - {name: http, containerPort: 8080}
      resources:
        requests:
          cpu: 100m        # required: utilisation is measured against this
```

Four things worth knowing:

**Requests are mandatory.** Every resource a target names must have a request
on every container, or admission rejects the WebApp. Without it the HPA reports
`<unknown>` forever, which is a worse failure than being told up front.

**`spec.replicas` is ignored** while autoscaling is enabled. The operator stops
sending the field so the HPA owns it and the two never fight.

**Targets are not capped at 100.** Utilisation is a ratio of requests, so `250`
is meaningful.

**Disabling it returns to `spec.replicas`.** Set that value *before* you
disable, or a 20-pod app drops to whatever `replicas` says — which defaults
to 1.

Memory works the same way, and setting only memory gives you a memory-only
autoscaler:

```yaml
  autoscaling:
    enabled: true
    maxReplicas: 10
    targetMemoryUtilizationPercentage: 80
```

`behavior` passes straight through for stabilisation windows and scaling
policies.

---

## Configuration and secrets

```yaml
      env:
        - name: LOG_LEVEL
          value: info
        - name: DB_PASSWORD
          valueFrom:
            secretKeyRef:
              name: app-secrets
              key: password
      envFrom:
        - configMapRef:
            name: app-config
        - secretRef:
            name: app-secrets
```

Standard Kubernetes `env` and `envFrom`. ConfigMap and Secret **volumes** are
not exposed — mount configuration through the environment instead.

---

## Probes

```yaml
      readinessProbe:
        httpGet:
          path: /healthz
          port: http
        initialDelaySeconds: 3
      livenessProbe:
        httpGet:
          path: /healthz
          port: http
        periodSeconds: 10
      startupProbe:
        httpGet:
          path: /healthz
          port: http
        failureThreshold: 30
```

Set a `readinessProbe` on anything you care about. `Ready` and `Available` are
derived from Deployment status, which is derived from pod readiness — with no
probe they report "running", not "serving".

---

## Writable scratch space

A read-only root filesystem is good practice, and it breaks any app that writes
to `/tmp`. That is what `scratchVolumes` is for:

```yaml
      securityContext:
        readOnlyRootFilesystem: true
      scratchVolumes:
        - name: tmp
          mountPath: /tmp
          sizeLimit: 64Mi
```

`emptyDir` only — up to 8 per container. `medium: Memory` gives you a tmpfs,
which **counts against the container's memory limit**, so `sizeLimit` is
required there.

Reserved paths are rejected: `/`, and anything under `/proc`, `/sys`, `/dev`,
`/etc`, `/var/run/secrets` or `/run/secrets`. So are unclean paths like
`/tmp/../etc` and trailing slashes.

---

## Sidecars

```yaml
spec:
  containers:
    - name: web
      image: ghcr.io/example/app:1.4.2
      ports: [{name: http, containerPort: 8080}]
      resources:
        requests: {cpu: 100m, memory: 128Mi}
    - name: metrics
      image: ghcr.io/example/exporter:0.27.1
      ports: [{name: metrics, containerPort: 9102}]
      resources:
        requests: {cpu: 10m, memory: 32Mi}
```

The Service exposes **every** port across **all** containers. Two rules bite
here:

- **Port names must be unique across the whole WebApp**, not per container,
  because Service ports share one namespace.
- **The same port number cannot appear twice** in one pod, on the same
  protocol. Containers share a network namespace, so the second bind fails at
  runtime.

Names must also be valid IANA service names — at least one letter, no
consecutive hyphens. `8080` is not a legal port name.

---

## Guardrails with WebAppPolicy

Cluster-scoped, evaluated at admission:

```yaml
apiVersion: webapps.example.com/v1alpha1
kind: WebAppPolicy
metadata:
  name: production-baseline
spec:
  enforcement: Audit          # start here
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
    minRequests: {cpu: 50m, memory: 64Mi}
    maxLimits:   {cpu: "4",  memory: 8Gi}
```

Roll out in three steps:

1. `Audit` — violations go to the operator log only. Look for
   `webapppolicy audit`.
2. `Warn` — violations come back as warnings on `kubectl apply`.
3. `Enforce` — violations reject the write.

Every matching policy is evaluated and violations accumulate. There is no
precedence: a stricter policy cannot be relaxed by a laxer one.

A resource named in `minRequests` or `maxLimits` must be **set** on every
container — an absent request is zero and an absent limit is unlimited, so
skipping them would exempt exactly the containers a bound is for.

> Namespace labels are editable by anyone with namespace write access. For a
> policy that must not be escapable, select on `kubernetes.io/metadata.name`,
> which the API server sets and protects.

---

## Deleting things

```
kubectl delete webapp hello
```

The operator removes the Ingress first so traffic stops being routed, then the
HPA, Service and Deployment, drops the metrics series, and only then clears its
finalizer. If any deletion fails the finalizer stays, so the WebApp remains
visible rather than vanishing with orphans behind it.

**Only objects the WebApp controls are touched.** A same-named Deployment that
something else owns is left alone.

To remove the operator:

```
make undeploy
```

This deletes every WebApp and WebAppPolicy first, so finalizers run while the
operator is still up, then removes the operator, the webhook configurations and
the CRDs together. The configurations must not outlive the Service they point
at — with `failurePolicy: Fail` they would reject every write.

---

## Common errors

**`webapp.webapps.example.com "..." is invalid: metadata.name: Invalid value`**
The name becomes a Service name and a label value: at most 63 characters, must
start with a letter, lowercase alphanumerics and hyphens only.

**Pods stuck `CreateContainerConfigError` or crash-looping immediately.**
Almost always a root image against the `restricted` profile. Use a non-root
image and a port above 1024.

**`spec.containers[0].resources.requests[cpu]: Required value`**
Autoscaling targets CPU, which is measured against requests.

**`port name must contain at least one letter (a-z)`**
`8080` is not a valid port name. Use `http`.

**`Degraded` with `already exists and is not controlled by this WebApp`**
Something else owns an object of that name in that namespace. Rename the
WebApp or remove the other object; the operator retries every minute.

**Everything rejected with `connect: connection refused` or a TLS error.**
The webhook is unreachable. Check the operator pods, that cert-manager issued
`webhook-server-cert`, and that `inject-ca-from` names the right namespace.

**Nothing is defaulted or validated.**
Webhooks are not installed, or the manager is running with
`-enable-webhooks=false`.

**`go build` fails with `does not implement runtime.Object`.**
Run `make generate`. `zz_generated.deepcopy.go` is not committed.

---

## What this operator will not do

Out of scope by design: TLS for application Ingresses, canary or blue-green
delivery, service mesh, stateful workloads and PersistentVolumes,
multi-cluster, weighted traffic routing, Jobs and CronJobs, NetworkPolicy.

Known gaps: no `v1` API or conversion webhook, `ingressURL` is always `http`,
one host and one path per Ingress with no annotation passthrough, no
PodDisruptionBudget or topology spread for the workloads themselves, and
`WebAppPolicy` is admission-time only except for the replica ceiling.

See [Limitations](README.md#limitations) and
[docs/decisions.md](docs/decisions.md).
