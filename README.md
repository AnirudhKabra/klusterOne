# klusterOne (`ko-controller`)

A Kubernetes-native operator + CLI for declaratively running **a user-supplied
script on a set of nodes** under safety constraints (cordon, optional drain,
max-unavailable budget, parallel "at-once" mode).

The unit of work is a single `NodeMaintenance` (`nm`) custom resource. You
attach a shell script to it, choose where it runs (a label selector, an
explicit node list, or every node), choose how aggressively it rolls out, and
the controller takes care of the rest — cordon, run the script on the host
via `nsenter`, capture exit code + logs into status, uncordon.

## What you get

- **CRD**: `nodemaintenances.ko.io` (cluster-scoped, short name `nm`).
- **Controller** (`ko-controller`): reconciles `NodeMaintenance` objects.
- **CLI** (`kubectl-nm`, plugin-style): create, attach, pause, run, status, logs.

## Quickstart

```bash
# 1. Install the CRD + controller
make install-crd
make build && ./bin/ko-controller --runner-namespace ko-system
# (in another shell)
make install-cli   # places kubectl-nm on $PATH (use DEST=... to relocate)
```

For an in-cluster deploy with a dedicated ServiceAccount and least-privilege
RBAC (no `delete` on `nodes`, runner Pods/ConfigMaps confined to `ko-system`):

```bash
make docker IMAGE=ghcr.io/kluster-one/ko-controller:v0.1.0
make deploy IMAGE=ghcr.io/kluster-one/ko-controller:v0.1.0
# tear down: make undeploy
```

See [`config/rbac/cluster_role.yaml`](./config/rbac/cluster_role.yaml) and
[`config/rbac/role.yaml`](./config/rbac/role.yaml) for the exact permissions
the operator runs with.

Once the controller is up (either way), drive it from the CLI:

```bash
# Run an ad-hoc script on every worker, max 2 unavailable at a time
kubectl nm create patch-kernel \
  --script ./scripts/01.sh \
  --selector node-role.kubernetes.io/worker= \
  --max-unavailable 2

# Watch progress
kubectl nm status patch-kernel
kubectl nm logs patch-kernel --node ip-10-0-1-7 -f
```

## Telling NMs apart at a glance

`kubectl get nm` shows a `Targets` column populated by the controller into
`.status.targets` during the first reconcile (so it works for NMs created
via `kubectl apply` as well as via the CLI), plus `Done`/`Total` sourced
from `status.summary`:

```text
NAME            PHASE       PAUSED  TARGETS                                   DONE  TOTAL  AGE
patch-kernel    InProgress  false   selector:node-role.kubernetes.io/worker=  4     12     4m
firmware-batch  Pending     true    nodes:node-7,node-8                       0     2      30s
fix-dns         Completed   false   all                                       18    18     1h
```

`kubectl get nm -o wide` adds per-phase counts (`Pending`, `InProgress`,
`Failed`). For the per-node breakdown of a specific run use
`kubectl nm status <name>`.

## Documentation

Deep-dive references live in [`docs/`](./docs/README.md):

| Topic                                          | Page                                             |
|------------------------------------------------|--------------------------------------------------|
| Components & data flow (CLI ↔ controller ↔ CRD)| [docs/architecture.md](./docs/architecture.md)   |
| Full `kubectl-nm` reference                    | [docs/cli.md](./docs/cli.md)                     |
| Reconcile lifecycle (3-node walkthrough)       | [docs/reconcile-flow.md](./docs/reconcile-flow.md) |
| How Script runs on the host                    | [docs/script-action.md](./docs/script-action.md) |
| Full CRD spec + codegen workflow               | [docs/crd-reference.md](./docs/crd-reference.md) |

## Layout

```
.
├── api/v1alpha1/                 # CRD Go types + deepcopy
├── cmd/
│   ├── manager/                  # ko-controller binary
│   └── kubectl-nm/               # kubectl plugin binary
├── config/
│   ├── crd/                      # CRD manifest
│   ├── manager/                  # Namespace + controller Deployment
│   ├── rbac/                     # ServiceAccount + (Cluster)Role(Binding)s
│   ├── samples/                  # Example NodeMaintenance objects
│   └── kustomization.yaml        # `kubectl apply -k config/` entry point
├── docs/                         # Architecture, CLI, reconcile flow, ...
├── internal/
│   ├── actions/                  # Cordon, Drain, Uncordon, Script
│   ├── cli/                      # kubectl-nm subcommands
│   ├── controller/               # controller-runtime reconciler
│   └── orchestrator/             # state machine + maxUnavailable/atOnce gate
├── bin/                          # local toolchain (controller-gen, build outputs) — gitignored
├── Dockerfile
├── Makefile
└── go.mod
```
