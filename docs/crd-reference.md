# CRD spec reference

The `nodemaintenances.ko.io` CRD is the single declarative interface
operators use to drive maintenance runs. This page documents the spec
shape and the codegen workflow you need when evolving the schema.

## Annotated spec

```yaml
apiVersion: ko.io/v1alpha1
kind: NodeMaintenance
metadata:
  name: rolling-patch
spec:
  paused: false
  allNodes: false                   # if true, ignores nodeSelector/nodeNames
  nodeSelector:                     # OR nodeNames, OR allNodes
    role: worker
  script:
    configMapRef:
      name: rolling-patch-script    # must live in the controller --runner-namespace
      key: script.sh                # defaults to "script.sh"
    # OR:
    # inline: |
    #   #!/bin/sh
    #   ...
    image: alpine:3.19              # default
    timeoutSeconds: 600
    runOnHost: true                 # default — nsenter into PID 1
    env:
      - { name: GREETING, value: hello }
  strategy:
    maxUnavailable: 2               # default 1
    atOnce: false                   # if true, overrides maxUnavailable
  actions:                          # defaults to [Cordon, Script, Uncordon]
    - type: Cordon
    - type: Drain
      drainOptions: { ignoreDaemonSets: true, timeoutSeconds: 300 }
    - type: Script
    - type: Uncordon
status:
  phase: InProgress                 # Pending | InProgress | Completed | Failed
  startTime: 2026-05-24T10:00:00Z
  summary:                          # per-phase counts; surfaced as printer columns
    total: 12
    pending: 7
    inProgress: 2
    completed: 3
    failed: 0
  nodes:
    - name: ip-10-0-1-7
      phase: InProgress
      currentAction: Script
      completedActions: [Cordon]
      scriptPodName: nm-rolling-patch-ip-10-0-1-7
      scriptExitCode: 0
      lastTransitionTime: 2026-05-24T10:00:42Z
```

For how `spec.script.runOnHost` changes the runner Pod, see
[script-action.md](./script-action.md). For how `spec.actions` is walked
during a run, see [reconcile-flow.md](./reconcile-flow.md).

## Field selection rules

The orchestrator resolves targets in this priority order; only one wins:

1. `spec.allNodes: true` — every Node in the cluster, label-selectorless.
2. `spec.nodeNames: [...]` — the explicit list, sorted lexicographically
   so reconciles are deterministic.
3. `spec.nodeSelector: {k: v, ...}` — every Node matching the (AND-ed)
   selector, also sorted.

Resolution happens **once**, at the first reconcile of the run, and the
result is frozen into `status.nodes[]`. Nodes added or removed from the
cluster mid-run do not change the target set — restart the NM if you need
to re-resolve.

`spec.actions` defaults to `[Cordon, Script, Uncordon]` when omitted and a
`spec.script` is attached. Set `spec.actions` explicitly when you want
`Drain` between `Cordon` and `Script`, or to omit `Cordon`/`Uncordon`
entirely.

## Updating the CRD after a spec change

When you add or change a field in `api/v1alpha1/nodemaintenance_types.go`,
two artifacts have to be regenerated in lockstep with the Go types:

- `api/v1alpha1/zz_generated.deepcopy.go` — `DeepCopy*` methods the
  controller needs to compile.
- `config/crd/ko.io_nodemaintenances.yaml` — the cluster-side schema (the
  API server silently drops fields not declared here).

Both are produced from kubebuilder markers
(`+kubebuilder:validation:*`, `+kubebuilder:printcolumn:*`, etc.) on the
Go types via `controller-gen`.

```bash
# 1. Edit api/v1alpha1/nodemaintenance_types.go — add the field and any markers
#    (e.g. +kubebuilder:validation:Minimum=1, +kubebuilder:validation:Enum=...).

# 2. Regenerate the deepcopy methods.
make generate

# 3. Regenerate the CRD schema.
make manifests

# 4. Apply the new CRD to the cluster and rebuild the controller / CLI.
make install-crd
make build install-cli
```

`controller-gen` is auto-downloaded into `./bin/controller-gen` on first
invocation; override the pinned version with
`make manifests CONTROLLER_GEN_VERSION=v0.17.0`.

## Adding a new ActionType

Adding a new action (say `Reboot`) is three edits:

1. Add the `ActionType` constant to `api/v1alpha1/nodemaintenance_types.go`
   (e.g. `ActionReboot ActionType = "Reboot"`) and any nested
   `*Options` struct it needs in `ActionSpec`.
2. Implement the action in `internal/actions/` (a struct with `Name()` and
   `Execute()` per the `Action` interface) — see existing `cordon.go` as
   a minimal example.
3. Wire it into the registry in `cmd/manager/main.go`:
   ```go
   registry.Register(kov1alpha1.ActionReboot, &actions.Reboot{...})
   ```

The orchestrator picks up the new type automatically — it does a registry
lookup keyed on `spec.Type`, so no orchestrator change is needed. After
the registration, run `make generate manifests install-crd build` to
ship the new type to the cluster.
