# Architecture

How `klusterOne` is laid out across the CLI, the controller, and the CRD,
plus the invariants that make the orchestrator reasonable to operate.

For the on-the-ground reconcile-by-reconcile walkthrough, see
[reconcile-flow.md](./reconcile-flow.md).

## Components & data flow

```mermaid
flowchart LR
    user(["User"])

    subgraph CLI["kubectl-nm (CLI)"]
        cli_create["create"]
        cli_attach["attach"]
        cli_pause["pause"]
        cli_run["run"]
        cli_status["status"]
        cli_logs["logs"]
    end

    subgraph API["Kubernetes API Server"]
        nm[("NodeMaintenance CR<br/>(spec.script.inline + status)")]
        cm[("ConfigMap<br/>nm-&lt;name&gt;-script<br/>(controller-managed)")]
        nodes[("Nodes")]
        pods[("Runner Pods<br/>(ko-system ns)")]
    end

    subgraph CTRL["ko-controller (manager binary)"]
        rec["NodeMaintenanceReconciler<br/>controller-runtime"]
        orch["Orchestrator<br/>state machine + budget"]
        reg["Action Registry"]

        subgraph ACT["Actions"]
            cordon["Cordon"]
            drain["Drain"]
            script["Script"]
            uncordon["Uncordon"]
        end
    end

    user --> CLI
    cli_create -->|create/update spec.script.inline| nm
    cli_attach -->|patch spec.script.inline| nm
    cli_pause -->|patch spec.paused=true<br/>+ ko.io/pause-reason annotation| nm
    cli_run -->|patch spec.paused=false<br/>clears pause-reason| nm
    cli_status -->|get| nm
    cli_logs -->|get logs| pods

    nm -. watch .-> rec
    rec --> orch
    orch --> reg
    reg --> cordon & drain & script & uncordon

    cordon -->|patch unschedulable=true| nodes
    uncordon -->|patch unschedulable=false| nodes
    drain -->|policyv1 Eviction| pods
    rec -->|"render spec.script.inline<br/>every reconcile, paused included"| cm
    script -->|defensive re-sync| cm
    script -->|"create pinned Pod<br/>mounts cm"| pods
    script -. nsenter into PID 1 .-> nodes
    rec -->|Status.Update| nm
```

The system has three independently-evolving pieces:

- **`kubectl-nm` CLI** — does plain API-server writes against
  `NodeMaintenance` CRs only (script body lives on `spec.script.inline`,
  pause/run patches on `spec.paused`). The CLI never writes ConfigMaps
  in `ko-system`; the controller materializes them. See
  [cli.md](./cli.md) for the full reference and
  [security.md](./security.md) for the trust boundary.
- **`ko-controller`** — a controller-runtime manager watching
  `NodeMaintenance`. The reconciler is intentionally thin; it delegates one
  **Step** to the orchestrator per reconcile.
- **Action Registry** — pluggable units (`Cordon`, `Drain`, `Uncordon`,
  `Script`) keyed by `ActionType`. The orchestrator never knows what an
  action *does* — only that `Execute` either succeeds or fails for one
  node. See [script-action.md](./script-action.md) for how the Script
  action in particular materializes its runner Pod.

## What happens in one reconcile

```mermaid
sequenceDiagram
    participant API as API Server
    participant R as Reconciler<br/>(controller-runtime)
    participant O as Orchestrator
    participant A as Action (e.g. Script)
    participant K as Cluster<br/>(Node / Pod)

    API->>R: Watch event on NodeMaintenance
    R->>API: Get nm
    alt nm terminal (Completed/Failed)
        R-->>API: nothing to do
    else
        opt status empty (first reconcile)
            R->>O: Init(ctx, nm)
            Note over O: initStatus<br/>resolveNodes → seed Pending<br/>stamp Phase / Targets / StartTime
            R->>API: Status().Update(nm)
            R-->>API: Result{Requeue: true}
        end
        opt spec.script != nil
            R->>API: EnsureScriptConfigMap
            Note over R: writes nm-NAME-script in ko-system<br/>idempotent — runs even when paused<br/>so the CM is inspectable pre-launch
        end
        alt nm paused
            R-->>API: Result{RequeueAfter: 15s}
        else
            R->>O: Step(ctx, nm)
            Note over O: admit Pending → InProgress<br/>(respects maxUnavailable / atOnce)
            loop for each InProgress node
                O->>O: idx = len(CompletedActions)
                O->>API: Get Node
                O->>A: Execute(ctx, nm, node, ns, spec)
                A->>K: Patch / Evict / Create Pod
                A-->>O: nil (advance) or error (fail node)
                O->>O: Append action to CompletedActions<br/>or mark node Failed
            end
            Note over O: rollup<br/>per-node phases → run phase<br/>+ status.summary counts
            O-->>R: requeue?
            R->>API: Status().Update(nm)
            R-->>API: Result{RequeueAfter: 10s}
        end
    end
```

Two important invariants come out of this loop:

- **One action per node per Step.** `advanceNode` runs exactly one action
  then returns, so even a `[Cordon, Drain, Script, Uncordon]` chain takes
  four reconciles per node. This keeps the status update small and lets
  the budget rebalance between steps.
- **Actions must be idempotent.** Crashes, conflict retries, or controller
  restarts will re-Execute a half-finished action. That's why every action
  checks "am I already in the desired state?" before mutating (`Cordon`
  looks at `node.Spec.Unschedulable`, `Script` `Get`s the pod by
  deterministic name and reuses it, `Drain` re-lists and re-evicts).

> For the same loop traced over six reconciles with concrete state
> snapshots and per-tick `kubectl get nm` output, see
> [reconcile-flow.md](./reconcile-flow.md).

## Per-node phase lifecycle

```mermaid
stateDiagram-v2
    direction LR
    [*] --> Pending: initStatus seeds<br/>status.nodes[]
    Pending --> InProgress: admit<br/>budget available
    InProgress --> InProgress: advanceNode<br/>append CompletedAction
    InProgress --> Completed: len of CompletedActions<br/>equals len of plan
    InProgress --> Failed: action returned error
    Completed --> [*]
    Failed --> [*]
```

The run-level `status.phase` is just a `rollup` of these per-node phases:
`InProgress` while any node is non-terminal, `Failed` if any node ended
`Failed`, otherwise `Completed`. A `Failed` node intentionally **stops
mid-chain** — if `Drain` fails the `Script` and `Uncordon` after it are
skipped, so the cluster operator sees the cordon still in place as a "do
not auto-recover" signal.
