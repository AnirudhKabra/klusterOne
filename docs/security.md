# Security model

`klusterOne` runs operator-supplied scripts as root on each target node's
host PID 1 namespace. The script body is therefore a high-value artifact;
this page documents who can write to it.

## The boundary

The script body lives on `spec.script.inline` of the `NodeMaintenance` CR.
The controller materializes the backing `ConfigMap` (`nm-<name>-script`
in `ko-system`) on reconcile. **The CLI never writes ConfigMaps directly.**

That means:

- Operators only need `nodemaintenances.ko.io` RBAC to run scripts —
  no `configmaps.update / patch / delete` in `ko-system`.
- Every change to a script is a `spec` mutation on the CR: it bumps
  `metadata.generation` and shows up in the kube-apiserver audit log.
  Re-scripting is auditable, never silent.

A `ValidatingAdmissionPolicy`
(`config/admission/configmap_lockdown.yaml`) enforces the other half:
**every** ConfigMap CREATE / UPDATE / DELETE in `ko-system` is denied
unless the requester is one of two SAs — the controller itself
(`system:serviceaccount:ko-system:ko-controller-manager`) or
`system:serviceaccount:kube-system:root-ca-cert-publisher` (which
auto-publishes `kube-root-ca.crt` into every namespace; blocking it
would break kubelet's CA distribution). Everyone else, including
cluster-admins acting through a normal kubeconfig, gets `Forbidden`.

`ko-system` is a controller-private namespace by design: privileged Pod
Security is enabled there so the runner Pod can `nsenter` into host PID
1, no untrusted workloads belong in it, and now its ConfigMaps are
controller-only at admission.

## The threat this closes

Without this setup, anyone with `configmaps.update` on `ko-system` could
silently swap a script that a privileged operator is about to run — with
zero RBAC on `NodeMaintenance`. That's a fleet-wide root-on-host
privilege escalation behind an obscure namespace permission.

## Requirements

- Kubernetes **1.30+** for the stable
  `admissionregistration.k8s.io/v1` VAP API. On 1.28–1.29, swap `v1`
  for `v1beta1` in the policy file. On clusters with no VAP support
  (pre-1.26), remove `admission` from `config/kustomization.yaml` and
  rely on RBAC hygiene: don't bind `edit`/`admin` on `ko-system` to
  humans.

## Verify it's working

```bash
# Any ConfigMap create in ko-system, by any non-controller principal,
# should be rejected — labeled or not.
kubectl -n ko-system create configmap evil \
  --from-literal=note='this should never succeed'
# → Error from server (Forbidden): ko-system-configmap-lockdown ...

# The supported flow still works end-to-end:
kubectl nm create demo --inline 'echo hello'
# → nodemaintenance.ko.io/demo created
```

If the first command succeeds, the policy isn't installed:
`kubectl get validatingadmissionpolicy ko-system-configmap-lockdown`.

`kube-root-ca.crt` is the one CM that legitimately gets created in
`ko-system` by something other than the controller. It is published by
the kube-controller-manager subroutine
`system:serviceaccount:kube-system:root-ca-cert-publisher`, which the
policy whitelists explicitly. `kubectl -n ko-system get configmap
kube-root-ca.crt` should resolve normally on any cluster.
