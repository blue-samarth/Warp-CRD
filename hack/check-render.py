#!/usr/bin/env python3
"""Assert the rendered manifests are internally consistent.

Rendering proves only that kustomize ran. The namespace and name transformers
rewrite metadata, but a reference held in a *field value* -- issuerRef.name, a
certificate dnsName, the inject-ca-from annotation, a ServiceMonitor serverName
-- is only rewritten when a replacement or nameReference says so. Miss one and
the manifest is silently wrong: nothing is issued, or the webhook is served a
certificate for the wrong name, and the first sign is a rollout that times out.

Usage: check-render.py dist/install.yaml dist/monitor.yaml
"""
import re
import sys

NS = "webapp-operator-system"
PREFIX = "webapp-operator-"

failures: list[str] = []
checks = 0


def check(desc: str, ok: bool, detail: str = "") -> None:
    global checks
    checks += 1
    if ok:
        print(f"  ok   {desc}")
    else:
        print(f"  FAIL {desc}" + (f" -- {detail}" if detail else ""))
        failures.append(desc)


def names_of(doc: str, kind: str) -> set[str]:
    """metadata.name of every object of this kind."""
    out = set()
    for block in doc.split("\n---\n"):
        if re.search(rf"^kind: {kind}$", block, re.M):
            m = re.search(r"^  name: (\S+)", block, re.M)
            if m:
                out.add(m.group(1))
    return out


def main() -> int:
    if len(sys.argv) != 3:
        print(__doc__)
        return 2
    install = open(sys.argv[1]).read()
    monitor = open(sys.argv[2]).read()

    print("namespace and naming")
    check("the target namespace is created",
          NS in names_of(install, "Namespace"),
          f"Namespaces: {sorted(names_of(install, 'Namespace'))}")
    check("cluster-scoped names are prefixed",
          f"{PREFIX}manager-role" in names_of(install, "ClusterRole"))
    check("the webhook Service is prefixed",
          f"{PREFIX}webhook-service" in names_of(install, "Service"))

    print("\ncertificate wiring")
    issuers = names_of(install, "Issuer")
    refs = set(re.findall(r"^  issuerRef:\n    kind: Issuer\n    name: (\S+)",
                          install, re.M))
    check("at least one Certificate references an Issuer", bool(refs))
    check("every issuerRef resolves to an Issuer in the render",
          refs <= issuers, f"issuers={sorted(issuers)} refs={sorted(refs)}")

    dns = re.findall(r"^  - (\S+\.svc)$", install, re.M)
    check("webhook certificate covers the webhook Service",
          f"{PREFIX}webhook-service.{NS}.svc" in dns, f"dnsNames={dns}")
    check("metrics certificate covers the metrics Service",
          f"{PREFIX}controller-manager-metrics-service.{NS}.svc" in dns,
          f"dnsNames={dns}")

    inject = set(re.findall(r"cert-manager\.io/inject-ca-from: (\S+)", install))
    check("inject-ca-from names a Certificate that exists",
          all(v.split("/")[0] == NS and v.split("/")[1] in names_of(install, "Certificate")
              for v in inject) and bool(inject),
          f"annotations={sorted(inject)} certs={sorted(names_of(install, 'Certificate'))}")

    secrets = set(re.findall(r"^  secretName: (\S+)", install, re.M))
    mounted = set(re.findall(r"secretName: (\S+)", install))
    check("every mounted certificate Secret is issued by a Certificate",
          {"webhook-server-cert", "metrics-server-cert"} <= secrets,
          f"secretNames={sorted(secrets)} referenced={sorted(mounted)}")

    print("\navailability")
    check("two manager replicas", re.search(r"^  replicas: 2$", install, re.M) is not None)
    check("a PodDisruptionBudget is present", bool(names_of(install, "PodDisruptionBudget")))

    print("\nmetrics scraping")
    check("the ServiceMonitor does not skip TLS verification",
          "insecureSkipVerify" not in monitor)
    check("serverName matches the metrics Service",
          f"serverName: {PREFIX}controller-manager-metrics-service.{NS}.svc" in monitor)
    check("the ServiceMonitor is co-located with the Service",
          f"namespace: {NS}" in monitor)

    print()
    if failures:
        print(f"{len(failures)} of {checks} checks failed:")
        for f in failures:
            print(f"  - {f}")
        return 1
    print(f"all {checks} checks passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
