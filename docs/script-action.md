# How the Script action works

The `Script` action is the workhorse of `klusterOne`. It is the action that
actually runs operator-supplied shell code on each target node, while the
surrounding `Cordon`/`Drain`/`Uncordon` actions provide the safety
envelope. This page documents how that runner Pod is built and how its
lifecycle is observed.

For where Script sits in the action chain and how the orchestrator drives
it, see [architecture.md](./architecture.md) and
[reconcile-flow.md](./reconcile-flow.md).

## Runner Pod construction

For each in-flight node, the controller materializes a privileged runner
Pod with:

- `spec.nodeName: <target>` — bypasses the scheduler. This is important
  because the node is already cordoned by the time `Script` runs, and a
  scheduled Pod would refuse to land on it.
- `tolerations: [{operator: Exists}]` — so it lands on tainted/cordoned
  nodes regardless of what's on them.
- `hostPID`, `hostNetwork`, `hostIPC` — set when `runOnHost: true`
  (default). These give the runner the same kernel namespaces as the host,
  which is what makes `nsenter` work.
- An **init container** that copies the script from the ConfigMap onto a
  hostPath directory (`/var/lib/ko-controller/scripts/<id>.sh` by default).
- A **main container** that runs:

  ```text
  nsenter --target 1 --mount --uts --ipc --net --pid \
          -- /bin/sh /var/lib/ko-controller/scripts/<id>.sh
  ```

  This is what makes the script effectively execute on the host itself,
  with access to the host's filesystem, processes, network, and so on —
  not inside the container's restricted namespaces.

## Lifecycle and status capture

`Script.Execute` blocks until the Pod reaches `Succeeded` or `Failed`. On
the way through, it records:

- `status.nodes[*].scriptPodName` — the deterministic Pod name (so
  `kubectl nm logs` can find it without listing).
- `status.nodes[*].scriptExitCode` — the per-node exit code from the
  runner Pod's container status.
- `status.nodes[*].message` — on failure, the last log chunk captured
  from the Pod, for quick triage from `kubectl nm status`.

A failed Script leaves the node **cordoned**. The trailing `Uncordon`
action in the chain is skipped (action chains stop at the first failure
per node, per the per-node phase rules in
[architecture.md](./architecture.md#per-node-phase-lifecycle)). This is
intentional: an operator looking at a Failed run should see the cluster
in a "needs attention" state, not silently recovered.

## When to use `runOnHost: false`

The default `runOnHost: true` is what you want for anything that needs to
touch the host directly — kernel patches, kubelet config edits, firmware
flashes, on-disk file repairs.

Pass `runOnHost: false` (CLI: `--in-pod`) to keep execution inside the
Pod's own namespaces. This is useful for:

- **API-side scripts** that only need a kubeconfig and don't touch the
  host. Faster startup, no `nsenter` overhead.
- **Read-only diagnostics** where you don't want a script to be able to
  modify the host even by accident.
- **Distroless or scratch images** that don't ship `nsenter` and where
  building it in is more trouble than it's worth.

The same `Cordon`/`Drain`/`Uncordon` safety wrapper still applies — you
just lose host-level execution inside the Script step itself.

## Pod Security Admission interactions

Because the runner Pod uses `hostPID`/`hostNetwork`/`hostIPC` and runs a
container with `privileged: true`, the **namespace it runs in must allow
`privileged` Pod Security**:

```yaml
metadata:
  labels:
    pod-security.kubernetes.io/enforce: privileged
```

Set `--runner-namespace` on the controller (and `--namespace` on the CLI)
to a namespace that has this label. The default `ko-system` namespace,
when installed via the project's manifests, is already labelled this way.
