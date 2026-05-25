# `kubectl-nm` CLI reference

`kubectl-nm` is a kubectl plugin that wraps the common workflows for
authoring, attaching scripts to, pausing, and inspecting `NodeMaintenance`
objects. It never talks to the controller directly — every command is a
plain API-server write. For where it sits in the bigger picture, see
[architecture.md](./architecture.md).

## Subcommands

```text
kubectl nm create <name> [flags]            build & apply a NodeMaintenance
kubectl nm attach <name> <script>           overwrite the script ConfigMap for an existing NM
kubectl nm pause  <name> [--reason TEXT]    pause an in-flight NM (flips spec.paused=true)
kubectl nm run    <name>                    unpause an existing NM (flips spec.paused=false)
kubectl nm status <name>                    pretty-print phase + per-node table
kubectl nm logs   <name> [--node X] [-f]    stream runner-pod logs
```

Both `pause` and `run` are idempotent — re-running them when the NM is
already in the target state prints a friendly message and does not issue a
patch. `pause --reason "..."` stamps the operator-supplied reason onto the
NM as the `ko.io/pause-reason` annotation, which `kubectl nm status`
surfaces; `pause` without `--reason` and `run` both clear that annotation.

## `kubectl nm create` flags

| Flag                  | Meaning                                                              |
|-----------------------|----------------------------------------------------------------------|
| `--script PATH`       | Read script from a file (creates a ConfigMap in the runner namespace) |
| `--inline STR`        | Use `STR` as the script body (mutually exclusive with `--script`)    |
| `--all-nodes`         | Target every node in the cluster                                     |
| `--at-once`           | Run on all targeted nodes in parallel (overrides `--max-unavailable`)|
| `--max-unavailable N` | Maximum nodes in-flight (default 1)                                  |
| `--selector k=v,…`    | Label selector for target nodes                                      |
| `--nodes a,b,c`       | Explicit node names                                                  |
| `--cordon` / `--uncordon` | Wrap the script with Cordon/Uncordon actions (both default true) |
| `--drain`             | Insert a Drain action between Cordon and Script                      |
| `--timeout DURATION`  | Per-node script execution timeout (default 10m)                      |
| `--image IMG`         | Runner container image (default `alpine:3.19`)                       |
| `--in-pod`            | Run the script inside the runner Pod (skip `nsenter` to host)        |
| `--namespace NS`      | Runner namespace where the script ConfigMap is created (default `ko-system`) |
| `--paused`            | Create paused; flip with `kubectl nm run`                            |
| `--dry-run` / `-o`    | Print the generated NodeMaintenance YAML and exit                    |

See [script-action.md](./script-action.md) for what `--in-pod` and
`--image` actually change inside the runner Pod.

## Two-phase workflow (attach → run)

```bash
# Create the NM in paused mode (placeholder ConfigMap)
kubectl nm create rolling-patch --inline ':' --selector role=worker --paused

# Drop in (or replace) the real script later
kubectl nm attach rolling-patch ./scripts/01.sh

# Kick it off
kubectl nm run rolling-patch
```

`attach` only touches the backing ConfigMap; the NM object itself is
unchanged, so this is a safe operation while the run is paused.

## Halting an in-flight run

```bash
# Stop admitting new nodes and stop starting new actions. Whatever
# action is currently mid-flight on a node finishes; nothing new starts.
kubectl nm pause rolling-patch --reason "investigating node-7"

# Inspect / fix / re-attach a new script as needed
kubectl nm status rolling-patch
kubectl nm attach rolling-patch ./fixed.sh

# Resume from wherever each node left off
kubectl nm run rolling-patch
```

`pause` is a fence between actions, not an interrupt mid-action: a Script
Pod already running on a node will finish (success or fail); the pause
prevents the *next* action (e.g. Uncordon) and prevents other Pending
nodes from being admitted. To hard-stop a running script, additionally
delete the runner Pod after pausing.

The exact semantics of "fence between actions" come from the orchestrator's
one-action-per-Step invariant — see
[architecture.md](./architecture.md#what-happens-in-one-reconcile) and
[reconcile-flow.md](./reconcile-flow.md) for the underlying state machine.
