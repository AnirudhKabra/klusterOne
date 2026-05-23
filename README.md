# Fury Controller

A Kubernetes-native operator for declarative node lifecycle orchestration.
Replaces ad-hoc `kubectl cordon` / `kubectl drain` workflows with a CRD-driven,
safety-aware reconciler.

This repository contains a **basic** orchestration core: the CRD, the reconciler,
the per-node state machine, and three pluggable actions (`Cordon`, `Drain`,
`Uncordon`). Reboot, OS upgrade, and custom-script actions can be added by
implementing the `Action` interface and registering it in `cmd/manager/main.go`.

## Layout

```
.
├── api/v1alpha1/                 # CRD Go types + deepcopy
├── cmd/manager/                  # Manager entry point
├── config/
│   ├── crd/                      # CRD manifest
│   └── samples/                  # Example NodeMaintenance
├── internal/
│   ├── actions/                  # Pluggable action interface + impls
│   ├── controller/               # controller-runtime reconciler
│   └── orchestrator/             # State machine + maxUnavailable gate
├── Dockerfile
├── Makefile
└── go.mod
```

## How orchestration works

`NodeMaintenance` declares **what** to do; the controller decides **when** and
**how**:

1. **Resolve targets** — from `spec.nodeNames` (preferred) or `spec.nodeSelector`.
2. **Admit** — promote `Pending` nodes to `InProgress` while the count of
   in-flight nodes stays ≤ `spec.strategy.maxUnavailable` (default `1`).
3. **Advance** — for every `InProgress` node, run the next un-completed action
   from `spec.actions` (one action per reconcile pass per node).
4. **Roll up** — once every node reaches `Completed` / `Failed`, the run's
   top-level `status.phase` becomes terminal and reconciliation stops.

Each node carries its own phase and `completedActions` list, so the system is
crash-safe: a manager restart resumes from `status` without re-running already
completed actions.

## Sample CRD

```yaml
apiVersion: fury.io/v1alpha1
kind: NodeMaintenance
metadata:
  name: rolling-drain-workers
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  strategy:
    maxUnavailable: 1
  actions:
    - type: Cordon
    - type: Drain
      drainOptions:
        ignoreDaemonSets: true
        gracePeriodSeconds: 30
        timeoutSeconds: 300
    - type: Uncordon
```

## Run it

```bash
make tidy
make install-crd
make run                 # uses your current kubeconfig
kubectl apply -f config/samples/nodemaintenance.yaml
kubectl get nodemaintenance -w
```

## Adding a new action

1. Implement the `actions.Action` interface in `internal/actions/<name>.go`.
2. Add a new `ActionType` constant in `api/v1alpha1/nodemaintenance_types.go`
   and extend the CRD's `enum` list.
3. Register it in `cmd/manager/main.go` with `registry.Register(...)`.

The orchestrator picks it up automatically — no reconciler changes required.
