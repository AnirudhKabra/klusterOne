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
unless the requester is one of four service accounts. Everyone else,
including cluster-admins acting through a normal kubeconfig, gets
`Forbidden`:

| Service account                                    | Why it's exempt                                         |
|----------------------------------------------------|---------------------------------------------------------|
| `ko-system:ko-controller-manager`                  | Primary writer — materializes script + output CMs.      |
| `kube-system:root-ca-cert-publisher`               | Seeds `kube-root-ca.crt` into every namespace.          |
| `kube-system:generic-garbage-collector`            | Cascades ownerRef deletes (cleans up CMs when their NM is gone). |
| `kube-system:namespace-controller`                 | Cleans up objects inside a namespace on `kubectl delete ns`. |

The two `kube-system` cleanup SAs are exempt because the lockdown
otherwise breaks Kubernetes' own housekeeping: without
`generic-garbage-collector`, a deleted NM's script CM survives in
`ko-system` forever (the CM has a valid `ownerReference`, but the GC's
DELETE is Forbidden); without `namespace-controller`,
`kubectl delete ns ko-system` hangs in `Terminating` forever.

Neither widens the attack surface. GC only deletes dependents whose
owner UID is already absent, and CREATE is still blocked — so an
attacker can't plant a CM with a forged owner to make GC delete
something it shouldn't. Deleting a namespace is itself a cluster-admin
operation, so the namespace-controller exemption grants no new
privilege.

`ko-system` is a controller-private namespace by design: privileged Pod
Security is enabled there so the runner Pod can `nsenter` into host PID
1, no untrusted workloads belong in it, and its ConfigMaps are
controller-only at admission.

### Preflight a write (RBAC vs admission)

`kubectl auth can-i` only checks the *authorization* layer (RBAC,
Node, Webhook). A cluster-admin will get `yes` for `create/update/delete
configmaps -n ko-system` — but the write itself will still be Forbidden
by this VAP, which runs in the *admission* layer on top.

To preflight a write that might be VAP-gated, use server-side dry-run
(it actually runs admission):

```bash
kubectl -n ko-system patch cm <name> --type=merge \
  -p '{...}' --dry-run=server
```

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

`kube-root-ca.crt` is the one CM that legitimately gets *created* in
`ko-system` by something other than the controller — published by
`kube-system:root-ca-cert-publisher`, which the policy allowlists
explicitly. `kubectl -n ko-system get configmap kube-root-ca.crt`
should resolve normally on any cluster.

Deletes by `kube-system:generic-garbage-collector` are similarly
expected — that's how `kubectl delete nm <name>` cascades cleanup to
the backing `nm-<name>-script` ConfigMap.
